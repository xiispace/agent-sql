package pg

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

// maxParams is PostgreSQL's limit on bind parameters in one statement.
const maxParams = 65535

// sessionSettings make both sides render and parse values identically, so
// the text form read from the source is exactly what the target stores.
var sessionSettings = []string{
	"SET TIME ZONE 'UTC'",
	"SET DateStyle = 'ISO, YMD'",
	"SET IntervalStyle = 'postgres'",
	"SET extra_float_digits = 3",
	"SET bytea_output = 'hex'",
	"SET client_encoding = 'UTF8'",
}

// openSession opens a dedicated connection with the connection's config.
func (c *pgConn) openSession(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.ConnectConfig(ctx, c.conn.Config().Copy())
	if err != nil {
		return nil, classifyError(err)
	}
	for _, stmt := range sessionSettings {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			_ = conn.Close(ctx)
			return nil, classifyError(err)
		}
	}
	return conn, nil
}

// OpenSnapshotReader implements driver.CopySessioner: one REPEATABLE READ,
// READ ONLY transaction, so introspection and every table read see the
// source at one point in time.
func (c *pgConn) OpenSnapshotReader(ctx context.Context, schema string) (driver.SnapshotReader, error) {
	conn, err := c.openSession(ctx)
	if err != nil {
		return nil, err
	}
	for _, stmt := range []string{
		"BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY",
		// --where conditions resolve unqualified names in the schema.
		"SET LOCAL search_path = " + quoteIdent(schema),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			_ = conn.Close(ctx)
			return nil, classifyError(err)
		}
	}
	return &snapshotReader{conn: conn, schema: schema}, nil
}

type snapshotReader struct {
	conn   *pgx.Conn
	schema string
}

// IntrospectSchema implements driver.SnapshotReader. introspect narrows
// search_path to pg_catalog for the transaction; it is pointed back at the
// schema afterwards, so --where conditions resolve unqualified names there.
func (r *snapshotReader) IntrospectSchema(ctx context.Context, schema string, opts driver.IntrospectOpts) (*schemadiff.Model, error) {
	m, err := introspect(ctx, r.conn, schema, opts)
	if err != nil {
		return nil, err
	}
	if _, err := r.conn.Exec(ctx, "SET LOCAL search_path = "+quoteIdent(r.schema)); err != nil {
		return nil, classifyError(err)
	}
	return m, nil
}

// Settings implements driver.SnapshotReader. Both sides use the fixed
// sessionSettings; PostgreSQL has no sql_mode to carry over.
func (r *snapshotReader) Settings() driver.SessionSettings { return driver.SessionSettings{} }

func (r *snapshotReader) Read(ctx context.Context, req driver.ReadRequest) (driver.RawRows, error) {
	return readRaw(ctx, r.conn.PgConn(), r.schema, req, nil), nil
}

func (r *snapshotReader) Count(ctx context.Context, table, where string) (int64, error) {
	return countRows(ctx, r.conn, r.schema, table, where)
}

func (r *snapshotReader) Close() error {
	ctx := context.Background()
	_, _ = r.conn.Exec(ctx, "ROLLBACK")
	return r.conn.Close(ctx)
}

func qualify(schema, table string) string { return quoteIdent(schema) + "." + quoteIdent(table) }

// selectSQL builds the row SELECT. Key columns compared as bytes are ordered
// by their text form under the "C" collation, which is byte order: what
// the merge's byte comparison reproduces.
func selectSQL(schema string, req driver.ReadRequest) string {
	cols := make([]string, len(req.Columns))
	for i, c := range req.Columns {
		cols[i] = quoteIdent(c)
	}
	sql := "SELECT " + strings.Join(cols, ", ") + " FROM " + qualify(schema, req.Table)
	if strings.TrimSpace(req.Where) != "" {
		sql += " WHERE (" + req.Where + ")"
	}
	if len(req.OrderBy) > 0 {
		order := make([]string, len(req.OrderBy))
		for i, k := range req.OrderBy {
			if k.Kind == schemadiff.KeyBytes {
				order[i] = "(" + quoteIdent(k.Name) + ")::text COLLATE \"C\""
			} else {
				order[i] = quoteIdent(k.Name)
			}
		}
		sql += " ORDER BY " + strings.Join(order, ", ")
	}
	return sql
}

// readRaw streams rows in the text format: every value arrives as the
// server's own text, nil for NULL.
func readRaw(ctx context.Context, conn *pgconn.PgConn, schema string, req driver.ReadRequest, onClose func()) driver.RawRows {
	rr := conn.ExecParams(ctx, selectSQL(schema, req), nil, nil, nil, nil)
	return &rawRows{rr: rr, onClose: onClose}
}

type rawRows struct {
	rr      *pgconn.ResultReader
	cur     [][]byte
	err     error
	closed  bool
	onClose func()
}

func (r *rawRows) Next() bool {
	if r.closed || !r.rr.NextRow() {
		return false
	}
	vals := r.rr.Values()
	row := make([][]byte, len(vals))
	for i, v := range vals {
		if v != nil {
			row[i] = append(make([]byte, 0, len(v)), v...)
		}
	}
	r.cur = row
	return true
}

func (r *rawRows) Values() [][]byte { return r.cur }

func (r *rawRows) Err() error {
	if r.err == nil && r.closed {
		return nil
	}
	if r.err == nil {
		// The command's error surfaces when the reader is closed.
		_, r.err = r.rr.Close()
		r.closed = true
		if r.onClose != nil {
			r.onClose()
			r.onClose = nil
		}
	}
	if r.err != nil {
		return classifyError(r.err)
	}
	return nil
}

func (r *rawRows) Close() error {
	if !r.closed {
		_, r.err = r.rr.Close()
		r.closed = true
	}
	if r.onClose != nil {
		r.onClose()
		r.onClose = nil
	}
	return nil
}

func countRows(ctx context.Context, q queryer, schema, table, where string) (int64, error) {
	sql := "SELECT COUNT(*) FROM " + qualify(schema, table)
	if strings.TrimSpace(where) != "" {
		sql += " WHERE (" + where + ")"
	}
	var n int64
	if err := q.QueryRow(ctx, sql).Scan(&n); err != nil {
		return 0, classifyError(err)
	}
	return n, nil
}

// OpenBulkWriter implements driver.CopySessioner. Foreign key triggers are
// switched off with session_replication_role = replica where the role may
// do so (superuser or equivalent); otherwise rows are written with checks
// on, and copier's dependency order (parents first) keeps inserts valid.
func (c *pgConn) OpenBulkWriter(ctx context.Context, schema string, _ driver.SessionSettings) (driver.BulkWriter, error) {
	if c.readonly {
		return nil, errors.New("copy target connection is read-only", errors.FixableByHuman).WithHint(errors.HintReadOnly)
	}
	conn, err := c.openSession(ctx)
	if err != nil {
		return nil, err
	}
	w := &bulkWriter{c: c, conn: conn, schema: schema}
	// Every statement is schema-qualified; the path makes values that name
	// objects (regclass, regtype), read unqualified under the source
	// schema's path, resolve in the target schema.
	if _, err := conn.Exec(ctx, "SET search_path = "+quoteIdent(schema)); err != nil {
		_ = conn.Close(ctx)
		return nil, classifyError(err)
	}
	if _, err := conn.Exec(ctx, "SET session_replication_role = replica"); err == nil {
		w.checksOff = true
	}
	return w, nil
}

type bulkWriter struct {
	c         *pgConn
	conn      *pgx.Conn
	reader    *pgx.Conn
	schema    string
	checksOff bool
}

func (w *bulkWriter) Exec(ctx context.Context, stmt string) error {
	if _, err := w.conn.Exec(ctx, stmt); err != nil {
		return classifyError(err)
	}
	return nil
}

func (w *bulkWriter) Begin(ctx context.Context) error    { return w.Exec(ctx, "BEGIN") }
func (w *bulkWriter) Commit(ctx context.Context) error   { return w.Exec(ctx, "COMMIT") }
func (w *bulkWriter) Rollback(ctx context.Context) error { return w.Exec(ctx, "ROLLBACK") }
func (w *bulkWriter) MaxParams() int                     { return maxParams }
func (w *bulkWriter) ChecksOff() bool                    { return w.checksOff }

// Truncate empties the table. TRUNCATE refuses a table other tables
// reference, whatever the replication role, so that refusal (and only that:
// a lock timeout, a missing privilege or a cancelled context is reported as
// is) falls back to DELETE.
func (w *bulkWriter) Truncate(ctx context.Context, table string) error {
	_, err := w.conn.Exec(ctx, "TRUNCATE "+qualify(w.schema, table))
	if err == nil {
		return nil
	}
	if truncateRefused(err) {
		return w.Exec(ctx, "DELETE FROM "+qualify(w.schema, table))
	}
	return classifyError(err)
}

// truncateRefused reports whether TRUNCATE failed because a foreign key
// references the table: PostgreSQL reports 0A000 ("cannot truncate a table
// referenced in a foreign key constraint"), CockroachDB names the
// referencing table ("... is referenced by foreign key from table ...").
func truncateRefused(err error) bool {
	var pgErr *pgconn.PgError
	if !stderrors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "0A000" || strings.Contains(pgErr.Message, "referenced by foreign key")
}

func columnList(cols []*schemadiff.Column) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quoteIdent(c.Name)
	}
	return strings.Join(out, ", ")
}

// tuples renders "($1, $2), ($3, $4)" and flattens the row values. Values
// travel as untyped text parameters; the server resolves each against its
// target column, exactly as it would a literal.
func tuples(ncols int, rows [][][]byte) (string, [][]byte) {
	parts := make([]string, len(rows))
	params := make([][]byte, 0, len(rows)*ncols)
	n := 1
	for i, row := range rows {
		ph := make([]string, ncols)
		for j := range ph {
			ph[j] = fmt.Sprintf("$%d", n)
			n++
		}
		parts[i] = "(" + strings.Join(ph, ", ") + ")"
		params = append(params, row...)
	}
	return strings.Join(parts, ", "), params
}

func (w *bulkWriter) exec(ctx context.Context, sql string, params [][]byte) (int64, error) {
	tag, err := w.conn.PgConn().ExecParams(ctx, sql, params, nil, nil, nil).Close()
	if err != nil {
		return 0, classifyError(err)
	}
	return tag.RowsAffected(), nil
}

// overriding lets copied values into GENERATED ALWAYS identity columns,
// which both PostgreSQL and CockroachDB refuse without it (428C9).
func overriding(cols []*schemadiff.Column) string {
	for _, c := range cols {
		if c.Identity == "ALWAYS" {
			return " OVERRIDING SYSTEM VALUE"
		}
	}
	return ""
}

func (w *bulkWriter) Insert(ctx context.Context, table string, cols []*schemadiff.Column, rows [][][]byte) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	values, params := tuples(len(cols), rows)
	sql := fmt.Sprintf("INSERT INTO %s (%s)%s VALUES %s", qualify(w.schema, table), columnList(cols), overriding(cols), values)
	return w.exec(ctx, sql, params)
}

// Update implements driver.BulkWriter: one UPDATE per row, matched by key,
// pipelined in one round trip. Each parameter takes its column's type, so
// no casts (and no schema-qualified enum names) are needed.
func (w *bulkWriter) Update(ctx context.Context, table string, cols []*schemadiff.Column, key []*schemadiff.Column, rows [][][]byte) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	isKey := make(map[string]bool, len(key))
	for _, k := range key {
		isKey[k.Name] = true
	}
	var sets, conds []string
	var idx []int
	n := 1
	add := func(list *[]string, i int, c *schemadiff.Column) {
		*list = append(*list, fmt.Sprintf("%s = $%d", quoteIdent(c.Name), n))
		idx = append(idx, i)
		n++
	}
	for i, c := range cols {
		if !isKey[c.Name] {
			add(&sets, i, c)
		}
	}
	for _, k := range key {
		for i, c := range cols {
			if c.Name == k.Name {
				if len(sets) == 0 {
					add(&sets, i, c) // key-only table: a no-op SET
				}
				add(&conds, i, c)
			}
		}
	}
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE %s", qualify(w.schema, table), strings.Join(sets, ", "), strings.Join(conds, " AND "))
	batch := &pgconn.Batch{}
	for _, row := range rows {
		params := make([][]byte, len(idx))
		for i, j := range idx {
			params[i] = row[j]
		}
		batch.ExecParams(sql, params, nil, nil, nil)
	}
	results, err := w.conn.PgConn().ExecBatch(ctx, batch).ReadAll()
	if err != nil {
		return 0, classifyError(err)
	}
	var total int64
	for _, r := range results {
		if r.Err != nil {
			return total, classifyError(r.Err)
		}
		total += r.CommandTag.RowsAffected()
	}
	return total, nil
}

func (w *bulkWriter) Delete(ctx context.Context, table string, key []*schemadiff.Column, keys [][][]byte) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	sql, params := w.deleteSQL(table, key, keys)
	return w.exec(ctx, sql, params)
}

func (w *bulkWriter) deleteSQL(table string, key []*schemadiff.Column, keys [][][]byte) (string, [][]byte) {
	values, params := tuples(len(key), keys)
	lhs := columnList(key)
	if len(key) > 1 {
		lhs = "(" + lhs + ")"
	} else {
		values = strings.NewReplacer("(", "", ")", "").Replace(values)
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)", qualify(w.schema, table), lhs, values), params
}

// DeleteUnreferenced implements driver.ReferencedKeeper. The batch is tried
// under a savepoint; when a foreign key rejects it, rows are retried one by
// one and those still referenced are kept.
func (w *bulkWriter) DeleteUnreferenced(ctx context.Context, table string, key []*schemadiff.Column, keys [][][]byte) (int64, [][][]byte, error) {
	try := func(keys [][][]byte) (int64, bool, error) {
		if err := w.Exec(ctx, "SAVEPOINT agent_sql_delete"); err != nil {
			return 0, false, err
		}
		sql, params := w.deleteSQL(table, key, keys)
		tag, err := w.conn.PgConn().ExecParams(ctx, sql, params, nil, nil, nil).Close()
		if err == nil {
			return tag.RowsAffected(), true, w.Exec(ctx, "RELEASE SAVEPOINT agent_sql_delete")
		}
		var pgErr *pgconn.PgError
		if !stderrors.As(err, &pgErr) || pgErr.Code != "23503" { // foreign_key_violation
			return 0, false, classifyError(err)
		}
		// ROLLBACK TO keeps the savepoint; release it so kept rows do not
		// stack subtransactions.
		if err := w.Exec(ctx, "ROLLBACK TO SAVEPOINT agent_sql_delete"); err != nil {
			return 0, false, err
		}
		return 0, false, w.Exec(ctx, "RELEASE SAVEPOINT agent_sql_delete")
	}
	n, ok, err := try(keys)
	if err != nil || ok {
		return n, nil, err
	}
	var deleted int64
	var kept [][][]byte
	for _, k := range keys {
		n, ok, err := try([][][]byte{k})
		if err != nil {
			return deleted, kept, err
		}
		if !ok {
			kept = append(kept, k)
		}
		deleted += n
	}
	return deleted, kept, nil
}

func (w *bulkWriter) readConn(ctx context.Context) (*pgx.Conn, error) {
	if w.reader == nil {
		conn, err := w.c.openSession(ctx)
		if err != nil {
			return nil, err
		}
		// --where conditions resolve unqualified names in the schema.
		if _, err := conn.Exec(ctx, "SET search_path = "+quoteIdent(w.schema)); err != nil {
			_ = conn.Close(ctx)
			return nil, classifyError(err)
		}
		w.reader = conn
	}
	return w.reader, nil
}

// Read streams the target table from its own snapshot, which ends when the
// rows are closed.
func (w *bulkWriter) Read(ctx context.Context, req driver.ReadRequest) (driver.RawRows, error) {
	conn, err := w.readConn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return nil, classifyError(err)
	}
	end := func() { _, _ = conn.Exec(context.Background(), "ROLLBACK") }
	return readRaw(ctx, conn.PgConn(), w.schema, req, end), nil
}

func (w *bulkWriter) Count(ctx context.Context, table, where string) (int64, error) {
	conn, err := w.readConn(ctx)
	if err != nil {
		return 0, err
	}
	return countRows(ctx, conn, w.schema, table, where)
}

// AutoIncrement returns the next value of the table's serial or identity
// sequence; 0 when it has none.
func (w *bulkWriter) AutoIncrement(ctx context.Context, table string) (uint64, error) {
	conn, err := w.readConn(ctx)
	if err != nil {
		return 0, err
	}
	n, err := nextSequenceValue(ctx, conn, w.schema, table)
	if err != nil {
		return 0, classifyError(err)
	}
	return n, nil
}

// RaiseAutoIncrement makes the table's sequence hand out value next.
func (w *bulkWriter) RaiseAutoIncrement(ctx context.Context, table string, value uint64) error {
	seq, err := tableSequence(ctx, w.conn, w.schema, table)
	if err != nil {
		return classifyError(err)
	}
	if seq == "" {
		return nil
	}
	if _, err := w.conn.Exec(ctx, "SELECT setval($1::regclass, $2, false)", seq, int64(value)); err != nil {
		return classifyError(err)
	}
	return nil
}

func (w *bulkWriter) Close() error {
	ctx := context.Background()
	if w.reader != nil {
		_ = w.reader.Close(ctx)
	}
	return w.conn.Close(ctx)
}
