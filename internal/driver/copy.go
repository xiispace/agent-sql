package driver

import (
	"context"

	"github.com/shhac/agent-sql/internal/schemadiff"
)

// IntrospectOpts selects what IntrospectSchema reads beyond tables.
type IntrospectOpts struct {
	Objects schemadiff.ObjectSet
}

// SchemaIntrospector returns the full model of one schema, addressed by name
// rather than by the connection's default database. Used by `schema diff`
// and `copy`.
type SchemaIntrospector interface {
	// Dialect names the SQL dialect family ("mysql" for MySQL and MariaDB).
	// schema diff and copy only pair connections of the same dialect.
	Dialect() string
	// ServerIdentity identifies the server instance, so copy can refuse a
	// source and target that are the same schema on the same server.
	ServerIdentity(ctx context.Context) (string, error)
	IntrospectSchema(ctx context.Context, schema string, opts IntrospectOpts) (*schemadiff.Model, error)
}

// SessionMarker is an optional SchemaIntrospector extension for drivers
// whose ServerIdentity can differ between routes to one server (a Unix
// socket vs TCP, a proxy or service address). One side holds a marker in its
// session; the other asks whether a session of the same server and database
// holds it.
type SessionMarker interface {
	// HoldMarker takes a session-scoped marker identified by key. release
	// frees it.
	HoldMarker(ctx context.Context, key int64) (release func(), err error)
	// MarkerHeld reports whether another session holds the marker key.
	MarkerHeld(ctx context.Context, key int64) (bool, error)
}

// ReferencedKeeper is an optional BulkWriter extension for writers that
// keep foreign key checks on. DeleteUnreferenced deletes the keys no foreign
// key still references and returns the ones it kept, so sync can delete
// target-only rows before the data pass and finish after it, once children
// that moved to another parent no longer point at them.
type ReferencedKeeper interface {
	DeleteUnreferenced(ctx context.Context, table string, key []*schemadiff.Column, keys [][][]byte) (deleted int64, kept [][][]byte, err error)
}

// DDLRenderer turns a classified diff into ordered, dialect-specific statements.
type DDLRenderer interface {
	RenderDDL(diff *schemadiff.Diff, opts schemadiff.RenderOpts) ([]schemadiff.Statement, error)
}

// CopySessioner opens the dedicated sessions copy needs: a consistent-snapshot
// read-only reader, and a writer that keeps session settings across statements.
type CopySessioner interface {
	OpenSnapshotReader(ctx context.Context, schema string) (SnapshotReader, error)
	OpenBulkWriter(ctx context.Context, schema string, settings SessionSettings) (BulkWriter, error)
}

// SessionSettings carries the source session's settings to the target, so
// values are interpreted the same way on both sides.
type SessionSettings struct {
	SQLMode string
}

// KeyColumn is one column of the key rows are ordered and matched by.
type KeyColumn struct {
	Name string
	Kind schemadiff.KeyKind
}

// ReadRequest describes one table read.
type ReadRequest struct {
	Table   string
	Columns []string
	// Where is a caller-supplied filter condition appended to the SELECT.
	Where string
	// OrderBy orders the rows by these key columns, in the order the
	// key kind compares (binary order for KeyBytes).
	OrderBy []KeyColumn
}

// RawRows streams rows as raw bytes, one []byte per column, nil for NULL.
// Values returned by Values stay valid after the next call to Next.
type RawRows interface {
	Next() bool
	Values() [][]byte
	Err() error
	Close() error
}

// SnapshotReader reads one schema at a single point in time, read-only.
type SnapshotReader interface {
	IntrospectSchema(ctx context.Context, schema string, opts IntrospectOpts) (*schemadiff.Model, error)
	Settings() SessionSettings
	Read(ctx context.Context, req ReadRequest) (RawRows, error)
	Count(ctx context.Context, table, where string) (int64, error)
	Close() error
}

// BulkWriter writes rows and DDL to one schema on a dedicated session with
// foreign key checks off where the server allows it (see ChecksOff). Each table's rows are written inside
// Begin/Commit. Read and Count use a separate read session, so a table can be
// read in key order while it is being written.
type BulkWriter interface {
	Exec(ctx context.Context, stmt string) error
	Begin(ctx context.Context) error
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
	Truncate(ctx context.Context, table string) error
	Insert(ctx context.Context, table string, cols []*schemadiff.Column, rows [][][]byte) (int64, error)
	// Update rewrites existing rows, matched by key, to the given values.
	// rows carry cols in order; the key columns are among them. Never an
	// INSERT ... ON DUPLICATE KEY / ON CONFLICT: those match any unique
	// index, so a row could land on an unrelated row's values.
	Update(ctx context.Context, table string, cols []*schemadiff.Column, key []*schemadiff.Column, rows [][][]byte) (int64, error)
	Delete(ctx context.Context, table string, key []*schemadiff.Column, keys [][][]byte) (int64, error)
	// MaxParams is the most bind parameters one statement may carry.
	MaxParams() int
	// ChecksOff reports whether the session writes with foreign key checks
	// off. When it cannot, copy orders its writes so every intermediate
	// state is consistent: parents filled before children, children
	// emptied before parents.
	ChecksOff() bool
	Read(ctx context.Context, req ReadRequest) (RawRows, error)
	Count(ctx context.Context, table, where string) (int64, error)
	AutoIncrement(ctx context.Context, table string) (uint64, error)
	RaiseAutoIncrement(ctx context.Context, table string, value uint64) error
	Close() error
}
