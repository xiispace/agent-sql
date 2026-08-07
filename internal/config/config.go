// Package config handles reading and writing the agent-sql configuration file.
// Config lives at $XDG_CONFIG_HOME/agent-sql/config.json (default ~/.config/agent-sql/).
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/shhac/lib-agent-cli/creds"
	"github.com/shhac/lib-agent-cli/xdg"
)

// Connection represents a saved database connection.
type Connection struct {
	Driver     string            `json:"driver"`
	Host       string            `json:"host,omitempty"`
	Port       int               `json:"port,omitempty"`
	Database   string            `json:"database,omitempty"`
	Path       string            `json:"path,omitempty"`
	URL        string            `json:"url,omitempty"`
	Credential string            `json:"credential,omitempty"`
	Account    string            `json:"account,omitempty"`
	Warehouse  string            `json:"warehouse,omitempty"`
	Role       string            `json:"role,omitempty"`
	Schema     string            `json:"schema,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
}

// Settings holds persistent configuration settings.
type Settings struct {
	Defaults   *DefaultsSettings   `json:"defaults,omitempty"`
	Query      *QuerySettings      `json:"query,omitempty"`
	Truncation *TruncationSettings `json:"truncation,omitempty"`
}

// DefaultsSettings holds default output settings.
type DefaultsSettings struct {
	Limit  *int   `json:"limit,omitempty"`
	Format string `json:"format,omitempty"`
}

// QuerySettings holds query execution settings.
type QuerySettings struct {
	Timeout *int `json:"timeout,omitempty"`
	MaxRows *int `json:"maxRows,omitempty"`
	// Format is the default output format for query commands (the csv-capable
	// class); it falls back to DefaultsSettings.Format when unset.
	Format string `json:"format,omitempty"`
}

// TruncationSettings holds string truncation settings.
type TruncationSettings struct {
	MaxLength *int `json:"maxLength,omitempty"`
}

// Config is the top-level configuration structure.
type Config struct {
	DefaultConnection string                `json:"default_connection,omitempty"`
	Connections       map[string]Connection `json:"connections"`
	Settings          Settings              `json:"settings"`
}

var (
	cache     *Config
	cacheMu   sync.Mutex
	configDir string
)

// ConfigDir returns the config directory, creating it if needed. The
// test-override short-circuit is kept; the default path now comes from the
// shared xdg helper so the whole family resolves $XDG_CONFIG_HOME identically.
func ConfigDir() string {
	if configDir != "" {
		return configDir
	}
	return xdg.ConfigDir("agent-sql")
}

// SetConfigDir overrides the config directory (for testing).
func SetConfigDir(dir string) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	configDir = dir
	cache = nil
}

func configPath() string {
	return filepath.Join(ConfigDir(), "config.json")
}

// store is the shared config file: 0600 writes into a 0700 parent, atomic
// replacement, and Update for a locked read-modify-write. Every StoreX/
// RemoveX/SetDefault/UpdateSetting mutator used to Read() then Write() as two
// separate steps, which raced across processes: two concurrent CLI
// invocations could each build their write from a snapshot taken before the
// other landed, and the loser's change was silently erased.
func store() creds.Store {
	return creds.Store{Path: configPath()}
}

// ClearCache forces a re-read on next access.
func ClearCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cache = nil
}

// Read returns the current configuration.
func Read() *Config {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cache != nil {
		return cache
	}

	data, err := os.ReadFile(configPath())
	if err != nil {
		return defaultConfig()
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig()
	}
	if cfg.Connections == nil {
		cfg.Connections = make(map[string]Connection)
	}
	cache = &cfg
	return cache
}

// Write saves the configuration to disk. It performs a plain save, not a
// read-modify-write — callers that need to mutate the existing config should
// go through update (via StoreConnection, RemoveConnection, SetDefault,
// UpdateSetting, or ResetSettings) instead of calling Read then Write
// directly, which reintroduces the lost-update race update exists to close.
func Write(cfg *Config) error {
	cacheMu.Lock()
	cache = nil
	cacheMu.Unlock()

	return store().Save(cfg)
}

// update runs a read-modify-write of the config file under one exclusive
// lock, so two concurrent CLI invocations serialize instead of each building
// its write from a snapshot taken before the other's landed. It reads FRESH
// state straight from disk — deliberately bypassing the in-memory cache,
// which may be empty or stale in this process — and invalidates the cache
// afterward so the next Read() picks up the write instead of serving what
// was cached before the lock was taken.
func update(mutate func(cfg *Config) error) error {
	cfg := defaultConfig()
	err := store().Update(cfg, func() error {
		if cfg.Connections == nil {
			cfg.Connections = make(map[string]Connection)
		}
		return mutate(cfg)
	})

	cacheMu.Lock()
	cache = nil
	cacheMu.Unlock()

	return err
}

// GetConnection returns a connection by alias, or nil if not found.
func GetConnection(alias string) *Connection {
	cfg := Read()
	conn, ok := cfg.Connections[alias]
	if !ok {
		return nil
	}
	return &conn
}

// GetConnections returns all connections.
func GetConnections() map[string]Connection {
	return Read().Connections
}

// GetDefaultAlias returns the default connection alias, or empty string.
func GetDefaultAlias() string {
	return Read().DefaultConnection
}

// StoreConnection saves a connection. First connection becomes the default.
func StoreConnection(alias string, conn Connection) error {
	return update(func(cfg *Config) error {
		cfg.Connections[alias] = conn
		if cfg.DefaultConnection == "" {
			cfg.DefaultConnection = alias
		}
		return nil
	})
}

// RemoveConnection removes a connection by alias.
func RemoveConnection(alias string) error {
	return update(func(cfg *Config) error {
		if _, ok := cfg.Connections[alias]; !ok {
			return &ConnectionNotFoundError{Alias: alias, Available: connectionAliases(cfg)}
		}
		delete(cfg.Connections, alias)
		if cfg.DefaultConnection == alias {
			cfg.DefaultConnection = ""
			for k := range cfg.Connections {
				cfg.DefaultConnection = k
				break
			}
		}
		return nil
	})
}

// SetDefault sets the default connection alias.
func SetDefault(alias string) error {
	return update(func(cfg *Config) error {
		if _, ok := cfg.Connections[alias]; !ok {
			return &ConnectionNotFoundError{Alias: alias, Available: connectionAliases(cfg)}
		}
		cfg.DefaultConnection = alias
		return nil
	})
}

// GetSetting reads a dotted config key (e.g. "query.timeout").
func GetSetting(key string) any {
	cfg := Read()
	parts := strings.Split(key, ".")
	var current any = settingsToMap(cfg.Settings)
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}
	return current
}

// UpdateSetting writes a dotted config key.
func UpdateSetting(key string, value any) error {
	return update(func(cfg *Config) error {
		parts := strings.Split(key, ".")
		m := settingsToMap(cfg.Settings)
		parent := m
		for _, part := range parts[:len(parts)-1] {
			child, ok := parent[part].(map[string]any)
			if !ok {
				child = make(map[string]any)
				parent[part] = child
			}
			parent = child
		}
		parent[parts[len(parts)-1]] = value
		cfg.Settings = mapToSettings(m)
		return nil
	})
}

// ResetSettings clears all settings to defaults.
func ResetSettings() error {
	return update(func(cfg *Config) error {
		cfg.Settings = Settings{}
		return nil
	})
}

// ConnectionNotFoundError is returned when a connection alias doesn't exist.
type ConnectionNotFoundError struct {
	Alias     string
	Available []string
}

func (e *ConnectionNotFoundError) Error() string {
	listing := "(none)"
	if len(e.Available) > 0 {
		listing = strings.Join(e.Available, ", ")
	}
	return "Unknown connection: \"" + e.Alias + "\". Valid: " + listing
}

func defaultConfig() *Config {
	return &Config{
		Connections: make(map[string]Connection),
		Settings:    Settings{},
	}
}

func connectionAliases(cfg *Config) []string {
	aliases := make([]string, 0, len(cfg.Connections))
	for k := range cfg.Connections {
		aliases = append(aliases, k)
	}
	return aliases
}

func settingsToMap(s Settings) map[string]any {
	data, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m == nil {
		m = make(map[string]any)
	}
	return m
}

func mapToSettings(m map[string]any) Settings {
	data, _ := json.Marshal(m)
	var s Settings
	_ = json.Unmarshal(data, &s)
	return s
}

// Display rendering (DisplayURL, EffectiveHost, EffectivePort, etc.) lives
// in display.go.
