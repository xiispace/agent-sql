package mysql

import (
	"context"
	"database/sql"
	stderrors "errors"
	"fmt"
	"strconv"
	"strings"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

// queryer is the query surface introspection needs; *sql.DB and *sql.Conn
// both satisfy it, so the snapshot reader introspects inside its transaction.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Dialect implements driver.SchemaIntrospector. MySQL and MariaDB share it.
func (c *mysqlConn) Dialect() string { return "mysql" }

// ServerIdentity implements driver.SchemaIntrospector: MySQL's server_uuid,
// unique per data directory, so two servers that share a hostname and port
// (docker-compose stacks with the same service name) are told apart.
// MariaDB has no server_uuid (error 1193) and falls back to hostname:port;
// HoldMarker/MarkerHeld then catch the same server reached by other routes.
func (c *mysqlConn) ServerIdentity(ctx context.Context) (string, error) {
	var id string
	err := c.db.QueryRowContext(ctx, "SELECT @@server_uuid").Scan(&id)
	if err == nil {
		return id, nil
	}
	var myErr *gomysql.MySQLError
	if !stderrors.As(err, &myErr) || myErr.Number != 1193 { // ER_UNKNOWN_SYSTEM_VARIABLE
		return "", classifyError(err)
	}
	if err := c.db.QueryRowContext(ctx, "SELECT CONCAT(@@hostname, ':', @@port)").Scan(&id); err != nil {
		return "", classifyError(err)
	}
	return id, nil
}

// markerLock names the user-level lock behind a same-server marker.
func markerLock(key int64) string { return fmt.Sprintf("agent_sql_%016x", uint64(key)) }

// HoldMarker implements driver.SessionMarker with a user-level lock
// (GET_LOCK). The lock belongs to one connection, so it is pinned until
// release; it is visible server-wide, which suits a schema's identity.
func (c *mysqlConn) HoldMarker(ctx context.Context, key int64) (func(), error) {
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return nil, classifyError(err)
	}
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", markerLock(key)).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, classifyError(err)
	}
	if got.Int64 != 1 {
		_ = conn.Close()
		return nil, errors.New("user lock for the same-server check is already held", errors.FixableByRetry)
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), "DO RELEASE_LOCK(?)", markerLock(key))
		_ = conn.Close()
	}, nil
}

// MarkerHeld implements driver.SessionMarker: IS_USED_LOCK names the
// holder's connection id, or NULL when no session on this server holds it.
func (c *mysqlConn) MarkerHeld(ctx context.Context, key int64) (bool, error) {
	var holder sql.NullInt64
	if err := c.db.QueryRowContext(ctx, "SELECT IS_USED_LOCK(?)", markerLock(key)).Scan(&holder); err != nil {
		return false, classifyError(err)
	}
	return holder.Valid, nil
}

// IntrospectSchema implements driver.SchemaIntrospector. It runs on one
// dedicated session so the information_schema statistics setting sticks.
func (c *mysqlConn) IntrospectSchema(ctx context.Context, schema string, opts driver.IntrospectOpts) (*schemadiff.Model, error) {
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return nil, classifyError(err)
	}
	defer func() { _ = conn.Close() }()
	prepareIntrospection(ctx, conn, c.variant)
	return introspect(ctx, conn, c.variant, schema, opts)
}

// prepareIntrospection makes MySQL 8 report live AUTO_INCREMENT values:
// information_schema.tables is otherwise cached for a day.
func prepareIntrospection(ctx context.Context, q queryer, variant string) {
	if variant == "mariadb" {
		return
	}
	// Absent before 8.0.3; nothing is cached there either.
	_, _ = q.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")
}

func introspect(ctx context.Context, q queryer, variant, schema string, opts driver.IntrospectOpts) (*schemadiff.Model, error) {
	var name string
	err := q.QueryRowContext(ctx,
		"SELECT SCHEMA_NAME FROM information_schema.schemata WHERE SCHEMA_NAME = ?", schema).Scan(&name)
	if err == sql.ErrNoRows {
		return nil, errors.New(fmt.Sprintf("schema %q not found or not visible to this credential", schema), errors.FixableByAgent).
			WithHint("list the visible databases with `schema databases`")
	}
	if err != nil {
		return nil, classifyError(err)
	}

	m := &schemadiff.Model{Schema: schema, RuleAliases: innodbRuleAliases}
	steps := []func(context.Context, queryer, string, *schemadiff.Model) error{
		readTables, readColumns, readForeignKeys,
	}
	for _, step := range steps {
		if err := step(ctx, q, schema, m); err != nil {
			return nil, classifyError(err)
		}
	}
	if err := readIndexes(ctx, q, variant, schema, m); err != nil {
		return nil, classifyError(err)
	}
	for _, t := range m.Tables {
		ddl, err := showCreate(ctx, q, "SHOW CREATE TABLE "+qualify(schema, t.Name), "Create Table")
		if err != nil {
			return nil, classifyError(err)
		}
		t.CreateSQL = ddl
		t.Partitioning = partitionClause(ddl)
	}
	if opts.Objects.Views {
		if err := readViews(ctx, q, schema, m); err != nil {
			return nil, classifyError(err)
		}
	}
	if opts.Objects.Routines {
		if err := readRoutines(ctx, q, schema, m); err != nil {
			return nil, classifyError(err)
		}
	}
	if opts.Objects.Triggers {
		if err := readTriggers(ctx, q, schema, m); err != nil {
			return nil, classifyError(err)
		}
	}
	if opts.Objects.Events {
		if err := readEvents(ctx, q, schema, m); err != nil {
			return nil, classifyError(err)
		}
	}
	return m, nil
}

func quoteIdent(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func qualify(schema, name string) string { return quoteIdent(schema) + "." + quoteIdent(name) }

// charsetOf derives a collation's character set from its name prefix
// ("utf8mb4_0900_ai_ci" → "utf8mb4").
func charsetOf(collation string) string {
	if i := strings.IndexByte(collation, '_'); i > 0 {
		return collation[:i]
	}
	return collation
}

func readTables(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.QueryContext(ctx, `
		SELECT TABLE_NAME, COALESCE(ENGINE, ''), COALESCE(TABLE_COLLATION, ''),
		       COALESCE(TABLE_COMMENT, ''), AUTO_INCREMENT
		FROM information_schema.tables
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'
		ORDER BY TABLE_NAME`, schema)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		t := &schemadiff.Table{}
		var ai sql.NullInt64
		if err := rows.Scan(&t.Name, &t.Engine, &t.Collation, &t.Comment, &ai); err != nil {
			return err
		}
		t.Charset = charsetOf(t.Collation)
		if ai.Valid {
			t.AutoIncrement = uint64(ai.Int64)
		}
		m.Tables = append(m.Tables, t)
	}
	return rows.Err()
}

func readColumns(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.QueryContext(ctx, `
		SELECT TABLE_NAME, COLUMN_NAME, ORDINAL_POSITION, COLUMN_TYPE, IS_NULLABLE,
		       COLUMN_DEFAULT, COALESCE(EXTRA, ''), COALESCE(CHARACTER_SET_NAME, ''),
		       COALESCE(COLLATION_NAME, ''), COALESCE(COLUMN_COMMENT, ''),
		       COALESCE(GENERATION_EXPRESSION, '')
		FROM information_schema.columns
		WHERE TABLE_SCHEMA = ?
		ORDER BY TABLE_NAME, ORDINAL_POSITION`, schema)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var table, nullable, extra string
		var dflt sql.NullString
		c := &schemadiff.Column{}
		if err := rows.Scan(&table, &c.Name, &c.Position, &c.Type, &nullable, &dflt, &extra,
			&c.Charset, &c.Collation, &c.Comment, &c.Generated); err != nil {
			return err
		}
		t := m.Table(table)
		if t == nil {
			continue // a view's column
		}
		c.Nullable = nullable == "YES"
		applyExtra(c, extra)
		if c.Generated == "" {
			c.Default, c.DefaultIsExpr = normalizeDefault(dflt, extra)
		}
		t.Columns = append(t.Columns, c)
	}
	return rows.Err()
}

// applyExtra parses information_schema.columns.EXTRA, which both servers
// fill with space-separated flags: auto_increment, on update <expr>,
// VIRTUAL/STORED/PERSISTENT GENERATED, INVISIBLE, DEFAULT_GENERATED.
func applyExtra(c *schemadiff.Column, extra string) {
	lower := strings.ToLower(extra)
	c.AutoIncrement = strings.Contains(lower, "auto_increment")
	c.Invisible = strings.Contains(lower, "invisible")
	c.GeneratedStored = strings.Contains(lower, "stored generated") || strings.Contains(lower, "persistent generated")
	if i := strings.Index(lower, "on update "); i >= 0 {
		c.OnUpdate = strings.TrimSpace(extra[i+len("on update "):])
	}
}

// normalizeDefault maps COLUMN_DEFAULT to (literal value | expression).
// MySQL reports literals unquoted and flags expressions DEFAULT_GENERATED;
// MariaDB quotes string literals, reports NULL defaults as the word NULL,
// and leaves expressions unquoted.
func normalizeDefault(d sql.NullString, extra string) (*string, bool) {
	if !d.Valid {
		return nil, false
	}
	v := d.String
	if strings.Contains(strings.ToUpper(extra), "DEFAULT_GENERATED") {
		return &v, true
	}
	if v == "NULL" {
		return nil, false
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		s := strings.ReplaceAll(v[1:len(v)-1], "''", "'")
		s = strings.ReplaceAll(s, `\\`, `\`)
		return &s, false
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return &v, false
	}
	if isCurrentTimestamp(v) || strings.HasSuffix(v, ")") {
		return &v, true
	}
	return &v, false
}

func isCurrentTimestamp(v string) bool {
	l := strings.ToLower(v)
	return strings.HasPrefix(l, "current_timestamp") || strings.HasPrefix(l, "now(") || strings.HasPrefix(l, "localtimestamp")
}

func readIndexes(ctx context.Context, q queryer, variant, schema string, m *schemadiff.Model) error {
	visible := "IS_VISIBLE = 'YES'"
	expr := "EXPRESSION"
	if variant == "mariadb" {
		visible = "IGNORED = 'NO'"
		expr = "NULL"
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`
		SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, COLUMN_NAME, SUB_PART,
		       COALESCE(COLLATION, 'A'), INDEX_TYPE, COALESCE(INDEX_COMMENT, ''),
		       %s, %s
		FROM information_schema.statistics
		WHERE TABLE_SCHEMA = ?
		ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, visible, expr), schema)
	if err != nil && variant == "mariadb" {
		// IGNORED is MariaDB 10.6+; older servers have no invisible indexes.
		rows, err = q.QueryContext(ctx, `
			SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, COLUMN_NAME, SUB_PART,
			       COALESCE(COLLATION, 'A'), INDEX_TYPE, COALESCE(INDEX_COMMENT, ''), 1, NULL
			FROM information_schema.statistics
			WHERE TABLE_SCHEMA = ?
			ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, schema)
	}
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var table, name, collation, typ, comment string
		var nonUnique int
		var column, expression sql.NullString
		var subPart sql.NullInt64
		var isVisible bool
		if err := rows.Scan(&table, &name, &nonUnique, &column, &subPart, &collation, &typ, &comment, &isVisible, &expression); err != nil {
			return err
		}
		t := m.Table(table)
		if t == nil {
			continue
		}
		ix := t.Index(name)
		if ix == nil {
			ix = &schemadiff.Index{Name: name, Primary: name == "PRIMARY", Unique: nonUnique == 0,
				Type: typ, Visible: isVisible, Comment: comment}
			t.Indexes = append(t.Indexes, ix)
		}
		part := schemadiff.IndexPart{Column: column.String, Desc: collation == "D", Expression: expression.String}
		if subPart.Valid {
			part.SubPart = int(subPart.Int64)
		}
		ix.Parts = append(ix.Parts, part)
	}
	return rows.Err()
}

func readForeignKeys(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.QueryContext(ctx, `
		SELECT rc.TABLE_NAME, rc.CONSTRAINT_NAME, rc.UPDATE_RULE, rc.DELETE_RULE,
		       kcu.COLUMN_NAME, kcu.REFERENCED_TABLE_SCHEMA, kcu.REFERENCED_TABLE_NAME,
		       kcu.REFERENCED_COLUMN_NAME
		FROM information_schema.referential_constraints rc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.CONSTRAINT_SCHEMA = rc.CONSTRAINT_SCHEMA
		 AND kcu.CONSTRAINT_NAME = rc.CONSTRAINT_NAME
		 AND kcu.TABLE_NAME = rc.TABLE_NAME
		WHERE rc.CONSTRAINT_SCHEMA = ?
		ORDER BY rc.TABLE_NAME, rc.CONSTRAINT_NAME, kcu.ORDINAL_POSITION`, schema)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var table, name, onUpdate, onDelete, column, refSchema, refTable, refColumn string
		if err := rows.Scan(&table, &name, &onUpdate, &onDelete, &column, &refSchema, &refTable, &refColumn); err != nil {
			return err
		}
		t := m.Table(table)
		if t == nil {
			continue
		}
		fk := t.ForeignKey(name)
		if fk == nil {
			fk = &schemadiff.ForeignKey{Name: name, RefSchema: refSchema, RefTable: refTable,
				OnUpdate: onUpdate, OnDelete: onDelete}
			t.ForeignKeys = append(t.ForeignKeys, fk)
		}
		fk.Columns = append(fk.Columns, column)
		fk.RefColumns = append(fk.RefColumns, refColumn)
	}
	return rows.Err()
}

// innodbRuleAliases: InnoDB enforces NO ACTION (and an unspecified rule)
// immediately, exactly like RESTRICT, so they compare equal.
var innodbRuleAliases = map[string]string{"": "RESTRICT", "NO ACTION": "RESTRICT"}

// partitionClause extracts the PARTITION BY clause from SHOW CREATE TABLE
// output, including MySQL's /*!50100 ... */ version-comment wrapper.
func partitionClause(ddl string) string {
	i := strings.Index(ddl, "PARTITION BY")
	if i < 0 {
		return ""
	}
	if j := strings.LastIndex(ddl[:i], "/*!"); j >= 0 && strings.TrimSpace(ddl[j:i]) != "" && !strings.Contains(ddl[j:i], "*/") {
		i = j
	}
	return strings.TrimSpace(ddl[i:])
}

// showCreate runs a SHOW CREATE statement and returns the named column. The
// column is NULL when the account may not see the definition.
func showCreate(ctx context.Context, q queryer, stmt, column string) (string, error) {
	rows, err := q.QueryContext(ctx, stmt)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%s returned no rows", stmt)
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", err
	}
	for i, c := range cols {
		if strings.EqualFold(c, column) {
			if !vals[i].Valid {
				return "", errors.New(fmt.Sprintf("%s: definition not visible to this credential", stmt), errors.FixableByHuman).
					WithHint("the account needs privileges to read object definitions (SHOW_ROUTINE or ownership)")
			}
			return vals[i].String, nil
		}
	}
	return "", fmt.Errorf("%s: no %q column", stmt, column)
}

func readViews(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.QueryContext(ctx, `
		SELECT TABLE_NAME, DEFINER FROM information_schema.views
		WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME`, schema)
	if err != nil {
		return err
	}
	var views []*schemadiff.View
	for rows.Next() {
		v := &schemadiff.View{}
		if err := rows.Scan(&v.Name, &v.Definer); err != nil {
			_ = rows.Close()
			return err
		}
		views = append(views, v)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, v := range views {
		ddl, err := showCreate(ctx, q, "SHOW CREATE VIEW "+qualify(schema, v.Name), "Create View")
		if err != nil {
			return err
		}
		v.CreateSQL = ddl
	}
	m.Views = views
	return nil
}

func readRoutines(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.QueryContext(ctx, `
		SELECT ROUTINE_NAME, ROUTINE_TYPE, DEFINER FROM information_schema.routines
		WHERE ROUTINE_SCHEMA = ? ORDER BY ROUTINE_TYPE, ROUTINE_NAME`, schema)
	if err != nil {
		return err
	}
	var routines []*schemadiff.Routine
	for rows.Next() {
		r := &schemadiff.Routine{}
		if err := rows.Scan(&r.Name, &r.Kind, &r.Definer); err != nil {
			_ = rows.Close()
			return err
		}
		routines = append(routines, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range routines {
		kind := strings.ToUpper(r.Kind)
		col := "Create Procedure"
		if kind == "FUNCTION" {
			col = "Create Function"
		}
		ddl, err := showCreate(ctx, q, "SHOW CREATE "+kind+" "+qualify(schema, r.Name), col)
		if err != nil {
			return err
		}
		r.CreateSQL = ddl
	}
	m.Routines = routines
	return nil
}

func readTriggers(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.QueryContext(ctx, `
		SELECT TRIGGER_NAME, EVENT_OBJECT_TABLE, DEFINER FROM information_schema.triggers
		WHERE TRIGGER_SCHEMA = ? ORDER BY EVENT_OBJECT_TABLE, ACTION_ORDER, TRIGGER_NAME`, schema)
	if err != nil {
		return err
	}
	var triggers []*schemadiff.Trigger
	for rows.Next() {
		t := &schemadiff.Trigger{}
		if err := rows.Scan(&t.Name, &t.Table, &t.Definer); err != nil {
			_ = rows.Close()
			return err
		}
		triggers = append(triggers, t)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range triggers {
		ddl, err := showCreate(ctx, q, "SHOW CREATE TRIGGER "+qualify(schema, t.Name), "SQL Original Statement")
		if err != nil {
			return err
		}
		t.CreateSQL = ddl
	}
	m.Triggers = triggers
	return nil
}

func readEvents(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.QueryContext(ctx, `
		SELECT EVENT_NAME, DEFINER FROM information_schema.events
		WHERE EVENT_SCHEMA = ? ORDER BY EVENT_NAME`, schema)
	if err != nil {
		return err
	}
	var events []*schemadiff.Event
	for rows.Next() {
		e := &schemadiff.Event{}
		if err := rows.Scan(&e.Name, &e.Definer); err != nil {
			_ = rows.Close()
			return err
		}
		events = append(events, e)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range events {
		ddl, err := showCreate(ctx, q, "SHOW CREATE EVENT "+qualify(schema, e.Name), "Create Event")
		if err != nil {
			return err
		}
		e.CreateSQL = ddl
	}
	m.Events = events
	return nil
}
