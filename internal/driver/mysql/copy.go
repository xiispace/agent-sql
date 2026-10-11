package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

// maxPlaceholders is MySQL's limit on bind parameters in one statement.
const maxPlaceholders = 65535

// openSession opens a dedicated single-connection pool cloned from the
// connection's configuration. Copy sessions read rows over the text protocol
// with parseTime off, so every value arrives as the server's own bytes
// (DATETIME(3), DECIMAL, JSON and zero dates round-trip unchanged). Writers
// interpolate parameters client-side, which saves a prepare round trip per
// batch.
func (c *mysqlConn) openSession(ctx context.Context, interpolate bool) (*sql.DB, *sql.Conn, error) {
	cfg := c.cfg.Clone()
	cfg.DBName = ""
	cfg.ParseTime = false
	cfg.InterpolateParams = interpolate
	cfg.MultiStatements = false
	connector, err := gomysql.NewConnector(cfg)
	if err != nil {
		return nil, nil, classifyError(err)
	}
	db := sql.OpenDB(connector)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, nil, classifyError(err)
	}
	if err := execAll(ctx, conn, sessionPrelude...); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, nil, err
	}
	return db, conn, nil
}

// sessionPrelude fixes the encoding and time zone of every copy session, so
// values move as the same bytes on both sides.
var sessionPrelude = []string{"SET NAMES utf8mb4", "SET SESSION time_zone = '+00:00'"}

// execAll runs stmts in order, stopping at the first error.
func execAll(ctx context.Context, conn *sql.Conn, stmts ...string) error {
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return classifyError(err)
		}
	}
	return nil
}

// OpenSnapshotReader implements driver.CopySessioner: one read-only session
// inside START TRANSACTION WITH CONSISTENT SNAPSHOT, so introspection and
// every table read see the source at one point in time.
func (c *mysqlConn) OpenSnapshotReader(ctx context.Context, schema string) (driver.SnapshotReader, error) {
	db, conn, err := c.openSession(ctx, false)
	if err != nil {
		return nil, err
	}
	r := &snapshotReader{db: db, conn: conn, variant: c.variant, schema: schema}
	prepareIntrospection(ctx, conn, c.variant)
	if err := execAll(ctx, conn,
		// --where conditions resolve unqualified names in the schema.
		"USE "+quoteIdent(schema),
		"SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ",
		"START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY",
	); err != nil {
		_ = r.Close()
		return nil, err
	}
	if err := conn.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&r.settings.SQLMode); err != nil {
		_ = r.Close()
		return nil, classifyError(err)
	}
	return r, nil
}

type snapshotReader struct {
	db       *sql.DB
	conn     *sql.Conn
	variant  string
	schema   string
	settings driver.SessionSettings
}

func (r *snapshotReader) IntrospectSchema(ctx context.Context, schema string, opts driver.IntrospectOpts) (*schemadiff.Model, error) {
	return introspect(ctx, r.conn, r.variant, schema, opts)
}

func (r *snapshotReader) Settings() driver.SessionSettings { return r.settings }

func (r *snapshotReader) Read(ctx context.Context, req driver.ReadRequest) (driver.RawRows, error) {
	return readRaw(ctx, r.conn, r.schema, req, nil)
}

func (r *snapshotReader) Count(ctx context.Context, table, where string) (int64, error) {
	return countRows(ctx, r.conn, r.schema, table, where)
}

func (r *snapshotReader) Close() error {
	_, _ = r.conn.ExecContext(context.Background(), "ROLLBACK")
	_ = r.conn.Close()
	return r.db.Close()
}

// selectSQL builds the row SELECT. The statement carries no bind
// parameters, so it runs over the text protocol.
func selectSQL(schema string, req driver.ReadRequest) string {
	cols := make([]string, len(req.Columns))
	for i, c := range req.Columns {
		cols[i] = quoteIdent(c)
	}
	sqlStr := "SELECT " + strings.Join(cols, ", ") + " FROM " + qualify(schema, req.Table)
	if strings.TrimSpace(req.Where) != "" {
		sqlStr += " WHERE (" + req.Where + ")"
	}
	if len(req.OrderBy) > 0 {
		order := make([]string, len(req.OrderBy))
		for i, k := range req.OrderBy {
			if k.Kind == schemadiff.KeyBytes {
				// Binary order is what a byte comparison reproduces; the
				// column's collation order is not.
				order[i] = "CAST(" + quoteIdent(k.Name) + " AS BINARY)"
			} else {
				order[i] = quoteIdent(k.Name)
			}
		}
		sqlStr += " ORDER BY " + strings.Join(order, ", ")
	}
	return sqlStr
}

func readRaw(ctx context.Context, conn *sql.Conn, schema string, req driver.ReadRequest, onClose func()) (driver.RawRows, error) {
	rows, err := conn.QueryContext(ctx, selectSQL(schema, req))
	if err != nil {
		if onClose != nil {
			onClose()
		}
		return nil, classifyError(err)
	}
	return &rawRows{rows: rows, n: len(req.Columns), onClose: onClose}, nil
}

func countRows(ctx context.Context, conn *sql.Conn, schema, table, where string) (int64, error) {
	sqlStr := "SELECT COUNT(*) FROM " + qualify(schema, table)
	if strings.TrimSpace(where) != "" {
		sqlStr += " WHERE (" + where + ")"
	}
	var n int64
	if err := conn.QueryRowContext(ctx, sqlStr).Scan(&n); err != nil {
		return 0, classifyError(err)
	}
	return n, nil
}

// rawRows scans each row into fresh byte slices: *[]byte keeps NULL (nil)
// apart from the empty string (non-nil, zero length), which sql.RawBytes
// does not.
type rawRows struct {
	rows    *sql.Rows
	n       int
	ptrs    []any // Scan targets, re-pointed at each row's vals
	cur     [][]byte
	err     error
	onClose func()
	closed  bool
}

func (r *rawRows) Next() bool {
	if r.err != nil || !r.rows.Next() {
		return false
	}
	vals := make([][]byte, r.n) // fresh per row: callers keep it
	if r.ptrs == nil {
		r.ptrs = make([]any, r.n)
	}
	for i := range vals {
		r.ptrs[i] = &vals[i]
	}
	if err := r.rows.Scan(r.ptrs...); err != nil {
		r.err = err
		return false
	}
	r.cur = vals
	return true
}

func (r *rawRows) Values() [][]byte { return r.cur }

func (r *rawRows) Err() error {
	if r.err != nil {
		return classifyError(r.err)
	}
	if err := r.rows.Err(); err != nil {
		return classifyError(err)
	}
	return nil
}

func (r *rawRows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.rows.Close()
	if r.onClose != nil {
		r.onClose()
	}
	return err
}

// OpenBulkWriter implements driver.CopySessioner. The writer session keeps
// foreign key checks off and an adapted sql_mode (see writerSQLMode) for its
// whole life; a
// second session serves key-ordered reads of the target. Unique checks stay
// on: append, upsert and sync write into populated tables, and with them off
// InnoDB may accept duplicates into a secondary unique index.
func (c *mysqlConn) OpenBulkWriter(ctx context.Context, schema string, settings driver.SessionSettings) (driver.BulkWriter, error) {
	if c.readonly {
		return nil, errors.New("copy target connection is read-only", errors.FixableByHuman).
			WithHint(errors.HintReadOnly)
	}
	db, conn, err := c.openSession(ctx, true)
	if err != nil {
		return nil, err
	}
	w := &bulkWriter{db: db, conn: conn, variant: c.variant, schema: schema}
	if err := execAll(ctx, conn, "SET SESSION FOREIGN_KEY_CHECKS = 0", "USE "+quoteIdent(schema)); err != nil {
		_ = w.Close()
		return nil, err
	}
	var targetMode string
	if err := conn.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&targetMode); err != nil {
		_ = w.Close()
		return nil, classifyError(err)
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_mode = ?", writerSQLMode(targetMode, settings.SQLMode)); err != nil {
		_ = w.Close()
		return nil, classifyError(err)
	}
	return w, nil
}

// parseModes are the sql_mode flags that change how SQL text parses. The
// DDL the writer replays is the source's (SHOW CREATE output quotes
// identifiers with " under ANSI_QUOTES; routine bodies are raw text), so
// these follow the source, whatever the target's own setting. Both MySQL
// and MariaDB know every one of them.
var parseModes = map[string]bool{
	"ANSI_QUOTES": true, "PIPES_AS_CONCAT": true, "IGNORE_SPACE": true,
	"REAL_AS_FLOAT": true, "HIGH_NOT_PRECEDENCE": true,
}

// writerSQLMode builds the writer session's sql_mode from the target
// session's own (so every flag is one the target server knows: a MariaDB
// or MySQL 5.7 source's NO_AUTO_CREATE_USER is refused by MySQL 8) and the
// source's parse modes. As mysqldump does, values already stored on the
// source (zero dates included) must be accepted as they are, and an explicit
// 0 in an AUTO_INCREMENT column must stay 0 rather than draw a new id.
// NO_BACKSLASH_ESCAPES is dropped too: the DDL the writer runs (quoteLiteral,
// and SHOW CREATE output, in every mode) escapes backslashes.
func writerSQLMode(target, source string) string {
	var out []string
	for _, m := range strings.Split(target, ",") {
		m = strings.TrimSpace(m)
		switch u := strings.ToUpper(m); {
		case parseModes[u]:
			continue
		case u == "", u == "NO_ZERO_DATE", u == "NO_ZERO_IN_DATE", u == "NO_AUTO_VALUE_ON_ZERO", u == "NO_BACKSLASH_ESCAPES":
			continue
		}
		out = append(out, m)
	}
	for _, m := range strings.Split(source, ",") {
		if m = strings.ToUpper(strings.TrimSpace(m)); parseModes[m] {
			out = append(out, m)
		}
	}
	return strings.Join(append(out, "NO_AUTO_VALUE_ON_ZERO"), ",")
}

type bulkWriter struct {
	db      *sql.DB
	conn    *sql.Conn
	reader  *sql.Conn
	variant string
	schema  string
}

func (w *bulkWriter) Exec(ctx context.Context, stmt string) error {
	if _, err := w.conn.ExecContext(ctx, stmt); err != nil {
		return classifyError(err)
	}
	return nil
}

func (w *bulkWriter) Begin(ctx context.Context) error { return w.Exec(ctx, "START TRANSACTION") }

func (w *bulkWriter) Commit(ctx context.Context) error { return w.Exec(ctx, "COMMIT") }

func (w *bulkWriter) Rollback(ctx context.Context) error { return w.Exec(ctx, "ROLLBACK") }

func (w *bulkWriter) Truncate(ctx context.Context, table string) error {
	return w.Exec(ctx, "TRUNCATE TABLE "+qualify(w.schema, table))
}

func (w *bulkWriter) MaxParams() int { return maxPlaceholders }

// ChecksOff implements driver.BulkWriter: FOREIGN_KEY_CHECKS = 0 is set for
// the whole session.
func (w *bulkWriter) ChecksOff() bool { return true }

func columnList(cols []*schemadiff.Column) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quoteIdent(c.Name)
	}
	return strings.Join(out, ", ")
}

// rowArgs turns raw values into bind arguments: bytes for binary columns,
// strings (in the session's utf8mb4) for everything else, nil for NULL.
func rowArgs(cols []*schemadiff.Column, rows [][][]byte) ([]any, string) {
	tuple := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
	tuples := make([]string, len(rows))
	args := make([]any, 0, len(rows)*len(cols))
	for i, row := range rows {
		tuples[i] = tuple
		for j, v := range row {
			switch {
			case v == nil:
				args = append(args, nil)
			case cols[j].IsBinary():
				args = append(args, v)
			default:
				args = append(args, string(v))
			}
		}
	}
	return args, strings.Join(tuples, ", ")
}

func (w *bulkWriter) Insert(ctx context.Context, table string, cols []*schemadiff.Column, rows [][][]byte) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	args, values := rowArgs(cols, rows)
	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s", qualify(w.schema, table), columnList(cols), values)
	return w.exec(ctx, stmt, args)
}

// Update implements driver.BulkWriter: one UPDATE per row, matched by key.
// (MySQL has no portable multi-row UPDATE ... FROM VALUES, and a derived
// table of strings would compare against the key by implicit conversion.)
func (w *bulkWriter) Update(ctx context.Context, table string, cols []*schemadiff.Column, key []*schemadiff.Column, rows [][][]byte) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	set, where, order := updateSQL(cols, key)
	stmt := fmt.Sprintf("UPDATE %s SET %s WHERE %s", qualify(w.schema, table), set, where)
	idx := positions(cols, order)
	var n int64
	for _, row := range rows {
		vals := make([][]byte, len(idx))
		for i, j := range idx {
			vals[i] = row[j]
		}
		args, _ := rowArgs(order, [][][]byte{vals})
		m, err := w.exec(ctx, stmt, args)
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}

// updateSQL renders the SET list (every non-key column; the key itself when
// there is none, as a no-op) and the key condition, and the column order the
// bind arguments follow: SET columns, then key columns.
func updateSQL(cols, key []*schemadiff.Column) (set, where string, order []*schemadiff.Column) {
	isKey := make(map[string]bool, len(key))
	for _, k := range key {
		isKey[k.Name] = true
	}
	var sets, conds []string
	for _, c := range cols {
		if !isKey[c.Name] {
			sets = append(sets, quoteIdent(c.Name)+" = ?")
			order = append(order, c)
		}
	}
	if len(sets) == 0 {
		sets = append(sets, quoteIdent(key[0].Name)+" = ?")
		order = append(order, key[0])
	}
	for _, k := range key {
		conds = append(conds, quoteIdent(k.Name)+" = ?")
		order = append(order, k)
	}
	return strings.Join(sets, ", "), strings.Join(conds, " AND "), order
}

// positions maps each of want to its index in cols.
func positions(cols, want []*schemadiff.Column) []int {
	pos := make(map[string]int, len(cols))
	for i, c := range cols {
		pos[c.Name] = i
	}
	out := make([]int, len(want))
	for i, c := range want {
		out[i] = pos[c.Name]
	}
	return out
}

func (w *bulkWriter) Delete(ctx context.Context, table string, key []*schemadiff.Column, keys [][][]byte) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	args, values := rowArgs(key, keys)
	lhs := columnList(key)
	if len(key) > 1 {
		lhs = "(" + lhs + ")"
	} else {
		// Single-column keys: IN (?, ?) rather than IN ((?), (?)).
		values = strings.NewReplacer("(", "", ")", "").Replace(values)
	}
	stmt := fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)", qualify(w.schema, table), lhs, values)
	return w.exec(ctx, stmt, args)
}

func (w *bulkWriter) exec(ctx context.Context, stmt string, args []any) (int64, error) {
	res, err := w.conn.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, classifyError(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// readConn returns the target read session, opening it on first use.
func (w *bulkWriter) readConn(ctx context.Context) (*sql.Conn, error) {
	if w.reader != nil {
		return w.reader, nil
	}
	conn, err := w.db.Conn(ctx)
	if err != nil {
		return nil, classifyError(err)
	}
	err = execAll(ctx, conn, sessionPrelude...)
	if err == nil {
		err = execAll(ctx, conn, "USE "+quoteIdent(w.schema))
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	prepareIntrospection(ctx, conn, w.variant)
	w.reader = conn
	return conn, nil
}

// Read streams the target table from its own consistent snapshot. The
// snapshot ends when the rows are closed.
func (w *bulkWriter) Read(ctx context.Context, req driver.ReadRequest) (driver.RawRows, error) {
	conn, err := w.readConn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "START TRANSACTION WITH CONSISTENT SNAPSHOT, READ ONLY"); err != nil {
		return nil, classifyError(err)
	}
	end := func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }
	return readRaw(ctx, conn, w.schema, req, end)
}

func (w *bulkWriter) Count(ctx context.Context, table, where string) (int64, error) {
	conn, err := w.readConn(ctx)
	if err != nil {
		return 0, err
	}
	return countRows(ctx, conn, w.schema, table, where)
}

func (w *bulkWriter) AutoIncrement(ctx context.Context, table string) (uint64, error) {
	conn, err := w.readConn(ctx)
	if err != nil {
		return 0, err
	}
	var ai sql.NullInt64
	err = conn.QueryRowContext(ctx,
		"SELECT AUTO_INCREMENT FROM information_schema.tables WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?",
		w.schema, table).Scan(&ai)
	if err != nil && err != sql.ErrNoRows {
		return 0, classifyError(err)
	}
	return uint64(ai.Int64), nil
}

func (w *bulkWriter) RaiseAutoIncrement(ctx context.Context, table string, value uint64) error {
	return w.Exec(ctx, fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", qualify(w.schema, table), value))
}

func (w *bulkWriter) Close() error {
	if w.reader != nil {
		_ = w.reader.Close()
	}
	_ = w.conn.Close()
	return w.db.Close()
}
