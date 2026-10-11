package errors

import (
	"fmt"
	"strings"

	out "github.com/shhac/lib-agent-output"
)

// QueryError is a classified database error with context for LLMs. It IS the
// family's shared output.Error (a type alias, not a wrapper): commands return
// it from RunE and libcli.Run recognizes it via output.As, so fixable_by and
// hint survive to stderr instead of being re-wrapped as a generic agent error.
type QueryError = out.Error

// New creates a QueryError.
func New(message string, fixableBy FixableBy) *QueryError {
	return out.New(message, fixableBy)
}

// Wrap creates a QueryError wrapping an underlying error.
func Wrap(err error, fixableBy FixableBy) *QueryError {
	return out.Wrap(err, fixableBy)
}

// As is a convenience wrapper for errors.As with QueryError.
func As(err error, target **QueryError) bool {
	return out.As(err, target)
}

// Prefix returns err with prefix prepended to its message ("--to: ...",
// "target: ...", "table orders: ..."), keeping a QueryError's fixable_by and
// hint. Unclassified errors are wrapped as fixable_by agent.
func Prefix(prefix string, err error) error {
	if err == nil {
		return nil
	}
	var qe *QueryError
	if As(err, &qe) {
		cp := *qe
		cp.Message = prefix + ": " + qe.Message
		return &cp
	}
	return Wrap(fmt.Errorf("%s: %w", prefix, err), FixableByAgent)
}

// ConnTip is the shared "ad-hoc connections" hint appended to connection
// lookup failures. It's defined once so the unknown-connection and
// no-connection-specified messages stay in sync across this package and the
// resolve package.
const ConnTip = "Tip: -c also accepts file paths (e.g. ./data.db) and connection URLs (e.g. postgres://user:pass@host/db)."

// NotFound creates a "not found" error for connections.
func NotFound(alias string, available []string) *QueryError {
	listing := "(none configured)"
	if len(available) > 0 {
		listing = strings.Join(available, ", ")
	}
	return New(
		fmt.Sprintf("Unknown connection '%s'. Available connections: %s. %s", alias, listing, ConnTip),
		FixableByAgent,
	)
}
