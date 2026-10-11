// Package copier orchestrates `copy`: it pairs a source and a target
// connection, plans the structure phase from a schemadiff.Diff, gates
// destructive work, applies DDL, and moves rows source → target in bounded
// batches (replace, append, upsert, or a key-ordered merge for sync). All
// dialect-specific work goes through the optional driver interfaces.
package copier

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
)

// Introspectors returns both sides' SchemaIntrospector, rejecting drivers that
// lack one and pairs of different dialect families.
func Introspectors(src, dst driver.Connection) (driver.SchemaIntrospector, driver.SchemaIntrospector, error) {
	si, ok := src.(driver.SchemaIntrospector)
	if !ok {
		return nil, nil, errors.New("--from connection does not support schema comparison", errors.FixableByAgent).WithHint(errors.HintSchemaCompare)
	}
	ti, ok := dst.(driver.SchemaIntrospector)
	if !ok {
		return nil, nil, errors.New("--to connection does not support schema comparison", errors.FixableByAgent).WithHint(errors.HintSchemaCompare)
	}
	if si.Dialect() != ti.Dialect() {
		return nil, nil, errors.New(
			fmt.Sprintf("--from is a %s connection and --to is a %s connection; cross-dialect comparison is not supported", si.Dialect(), ti.Dialect()),
			errors.FixableByAgent,
		).WithHint("pair connections of the same driver family: mysql with mariadb, or pg with pg")
	}
	return si, ti, nil
}

// SameTarget rejects a source and target that are the same schema on the
// same server.
func SameTarget(ctx context.Context, si, ti driver.SchemaIntrospector, schema, toSchema string) error {
	if schema != toSchema {
		return nil
	}
	a, err := si.ServerIdentity(ctx)
	if err != nil {
		return sideError("source", err)
	}
	b, err := ti.ServerIdentity(ctx)
	if err != nil {
		return sideError("target", err)
	}
	if a == b || sharedSession(ctx, si, ti) {
		return errors.New(
			fmt.Sprintf("--from and --to are the same schema %q on the same server (%s)", schema, a),
			errors.FixableByAgent,
		).WithHint("point --to at another server, or pass --to-schema to copy into a different schema")
	}
	return nil
}

// sharedSession catches what ServerIdentity misses when the two connections
// reach one server by different routes: the source holds a random marker and
// the target looks for it. A failed probe proves nothing, so it reports
// false and the identity comparison stands. (CockroachDB's advisory locks are
// compatibility no-ops that always succeed, so there it never reports true.)
func sharedSession(ctx context.Context, si, ti driver.SchemaIntrospector) bool {
	sm, ok1 := si.(driver.SessionMarker)
	tm, ok2 := ti.(driver.SessionMarker)
	if !ok1 || !ok2 {
		return false
	}
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return false
	}
	key := int64(binary.LittleEndian.Uint64(buf[:]))
	release, err := sm.HoldMarker(ctx, key)
	if err != nil {
		return false
	}
	defer release()
	held, err := tm.MarkerHeld(ctx, key)
	return err == nil && held
}

// sideError prefixes an error with the side it came from.
func sideError(side string, err error) error { return errors.Prefix(side, err) }
