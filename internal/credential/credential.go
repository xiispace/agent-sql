// Package credential handles reading and writing stored credentials.
// Credentials live at $XDG_CONFIG_HOME/agent-sql/credentials.json.
// On macOS, sensitive values are stored in the system Keychain.
package credential

import (
	"encoding/json"
	"path/filepath"

	"github.com/shhac/agent-sql/internal/config"
	"github.com/shhac/lib-agent-cli/creds"
)

// Credential represents stored authentication credentials.
type Credential struct {
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	WritePermission bool   `json:"writePermission"`
	KeychainManaged bool   `json:"keychainManaged,omitempty"`
}

type credentialEntry struct {
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	WritePermission bool   `json:"writePermission"`
	KeychainManaged bool   `json:"keychainManaged,omitempty"`
}

// keychainService owns the reverse-domain namespace for this CLI's secrets.
const keychainService = "app.paulie.agent-sql"

// MCPKeychainService is the Keychain service for the MCP server's local-OAuth
// secrets — the CLI's service plus a ".mcp" namespace, separate from the API creds.
func MCPKeychainService() string { return keychainService + ".mcp" }

var keychain = creds.NewKeychain(keychainService)

func keychainAvailable() bool { return keychain.Available() }

func credentialsPath() string {
	return filepath.Join(config.ConfigDir(), "credentials.json")
}

// store is the shared credential file: 0600 writes into a 0700 parent, atomic
// replacement, and Update for a locked read-modify-write. This used to be
// hand-rolled here with os.ReadFile/os.WriteFile, which carried a lost-update
// race: two concurrent writers (e.g. two `conn add` invocations) could each
// build their write from a snapshot taken before the other landed, and the
// loser's entry silently vanished. For a credential that has already been
// pushed to the OS keychain by the time the index write happens, a lost entry
// is worse than a lost write: the secret is still in the keychain, but
// nothing references it — `auth list` can't show it and `auth remove` can't
// delete it, since it looks the name up in the index first.
func store() creds.Store {
	return creds.Store{Path: credentialsPath()}
}

func readIndex() map[string]credentialEntry {
	entries := map[string]credentialEntry{}
	if err := store().Load(&entries); err != nil {
		return make(map[string]credentialEntry)
	}
	if entries == nil {
		entries = make(map[string]credentialEntry)
	}
	return entries
}

// updateIndex applies mutate to the index under an exclusive lock, so two
// concurrent credential writes serialize instead of clobbering each other.
func updateIndex(mutate func(entries map[string]credentialEntry) error) error {
	entries := map[string]credentialEntry{}
	return store().Update(&entries, func() error {
		if entries == nil {
			entries = make(map[string]credentialEntry)
		}
		return mutate(entries)
	})
}

// Get retrieves a credential by name. Returns nil if not found.
func Get(name string) *Credential {
	entries := readIndex()
	entry, ok := entries[name]
	if !ok {
		return nil
	}

	cred := &Credential{
		Username:        entry.Username,
		Password:        entry.Password,
		WritePermission: entry.WritePermission,
		KeychainManaged: entry.KeychainManaged,
	}

	// Try to read from Keychain
	if entry.KeychainManaged && keychainAvailable() {
		if kcCred := readKeychain(name); kcCred != nil {
			cred.Username = kcCred.Username
			cred.Password = kcCred.Password
		}
	}

	return cred
}

// Store saves a credential. On macOS, sensitive values go to Keychain.
func Store(name string, cred Credential) (storage string, err error) {
	entry := credentialEntry{
		Username:        cred.Username,
		Password:        cred.Password,
		WritePermission: cred.WritePermission,
	}
	storage = "file"

	if keychainAvailable() {
		if kcErr := writeKeychain(name, &cred); kcErr == nil {
			entry = credentialEntry{
				Username:        "__KEYCHAIN__",
				Password:        "__KEYCHAIN__",
				WritePermission: cred.WritePermission,
				KeychainManaged: true,
			}
			storage = "keychain"
		}
	}

	// The index write is the step that must not race: if the keychain took
	// the secret above, it already holds it by the time this runs, so an
	// entry lost to a concurrent writer would leave that secret referenced by
	// nothing.
	if err := updateIndex(func(entries map[string]credentialEntry) error {
		entries[name] = entry
		return nil
	}); err != nil {
		return "", err
	}
	return storage, nil
}

// Remove deletes a credential.
func Remove(name string) error {
	return updateIndex(func(entries map[string]credentialEntry) error {
		entry, ok := entries[name]
		if !ok {
			return &NotFoundError{Name: name}
		}
		if entry.KeychainManaged && keychainAvailable() {
			deleteKeychain(name)
		}
		delete(entries, name)
		return nil
	})
}

// List returns all credential names.
func List() []string {
	entries := readIndex()
	names := make([]string, 0, len(entries))
	for k := range entries {
		names = append(names, k)
	}
	return names
}

// NotFoundError is returned when a credential doesn't exist.
type NotFoundError struct {
	Name string
}

func (e *NotFoundError) Error() string {
	return "Credential not found: " + e.Name
}

// Keychain helpers (macOS only)

func readKeychain(name string) *Credential {
	secret, ok := keychain.Get(name)
	if !ok {
		return nil
	}
	var cred Credential
	if err := json.Unmarshal([]byte(secret), &cred); err != nil {
		return nil
	}
	return &cred
}

func writeKeychain(name string, cred *Credential) error {
	data, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return keychain.Set(name, string(data))
}

func deleteKeychain(name string) {
	_ = keychain.Delete(name)
}
