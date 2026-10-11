package pg

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

// queryer is the query surface introspection needs; *pgx.Conn and pgx.Tx
// both satisfy it, so the snapshot reader introspects inside its snapshot.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Dialect implements driver.SchemaIntrospector. PostgreSQL and CockroachDB
// share it.
func (c *pgConn) Dialect() string { return "postgres" }

// ServerIdentity implements driver.SchemaIntrospector. A PostgreSQL schema
// lives inside a database, so the database is part of the identity.
func (c *pgConn) ServerIdentity(ctx context.Context) (string, error) {
	return serverIdentity(ctx, c.conn)
}

func serverIdentity(ctx context.Context, q queryer) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT COALESCE(host(inet_server_addr()), 'local') || ':' ||
		current_setting('port') || '/' || current_database()`).Scan(&id)
	if err != nil {
		return "", classifyError(err)
	}
	var sysid string
	// The cluster's system identifier tells apart two servers behind the
	// same address (containers, proxies); it may be unreadable for the role.
	if err := q.QueryRow(ctx, "SELECT system_identifier::text FROM pg_control_system()").Scan(&sysid); err == nil {
		id = sysid + "@" + id
	}
	return id, nil
}

// HoldMarker implements driver.SessionMarker with a session-level advisory
// lock. Advisory locks are scoped to the database, which is part of a
// PostgreSQL schema's identity, and need no privileges.
func (c *pgConn) HoldMarker(ctx context.Context, key int64) (func(), error) {
	var ok bool
	if err := c.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil {
		return nil, classifyError(err)
	}
	if !ok {
		return nil, errors.New("advisory lock for the same-server check is already held", errors.FixableByRetry)
	}
	return func() {
		_, _ = c.conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
	}, nil
}

// MarkerHeld implements driver.SessionMarker: the lock is held elsewhere when
// this session cannot take it.
func (c *pgConn) MarkerHeld(ctx context.Context, key int64) (bool, error) {
	var ok bool
	if err := c.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil {
		return false, classifyError(err)
	}
	if ok {
		_, _ = c.conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", key)
	}
	return !ok, nil
}

// IntrospectSchema implements driver.SchemaIntrospector. The schema is a
// namespace inside the connection's database.
func (c *pgConn) IntrospectSchema(ctx context.Context, schema string, opts driver.IntrospectOpts) (*schemadiff.Model, error) {
	tx, err := c.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, classifyError(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	return introspect(ctx, tx, schema, opts)
}

// introspect reads the model inside the caller's transaction. search_path
// is narrowed to pg_catalog for the transaction, so every definition the
// server renders (view bodies, defaults, types, index and constraint
// definitions) names its schema explicitly and can be re-pointed at the
// target schema.
func introspect(ctx context.Context, q queryer, schema string, opts driver.IntrospectOpts) (*schemadiff.Model, error) {
	if _, err := q.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return nil, classifyError(err)
	}
	var exists bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema).Scan(&exists); err != nil {
		return nil, classifyError(err)
	}
	if !exists {
		return nil, errors.New(fmt.Sprintf("schema %q not found or not visible to this credential", schema), errors.FixableByAgent).
			WithHint("list the schemas with `run \"SELECT nspname FROM pg_namespace\"`, or the databases with `schema databases`")
	}
	m := &schemadiff.Model{Schema: schema}
	steps := []func(context.Context, queryer, string, *schemadiff.Model) error{
		readTables, readColumns, readIndexes, readConstraints, readSequences,
	}
	if opts.Objects.Views {
		steps = append(steps, readViews)
	}
	if opts.Objects.Routines {
		steps = append(steps, readRoutines)
	}
	if opts.Objects.Triggers {
		steps = append(steps, readTriggers)
	}
	// PostgreSQL has no scheduled events; opts.Objects.Events reads nothing.
	for _, step := range steps {
		if err := step(ctx, q, schema, m); err != nil {
			return nil, classifyError(err)
		}
	}
	var db string
	if err := q.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return nil, classifyError(err)
	}
	stripDatabase(m, db)
	return m, nil
}

// stripDatabase removes a database qualifier ("db.schema.table" →
// "schema.table") from server-rendered definitions. CockroachDB renders
// three-part names; PostgreSQL never does, so this is a no-op there.
func stripDatabase(m *schemadiff.Model, db string) {
	strip := func(s string) string {
		s = strings.ReplaceAll(s, quoteIdent(db)+".", "")
		return strings.ReplaceAll(s, " "+db+".", " ")
	}
	for _, t := range m.Tables {
		for _, ix := range t.Indexes {
			ix.Definition = strip(ix.Definition)
			ix.ConstraintDefinition = strip(ix.ConstraintDefinition)
		}
		for _, c := range t.Checks {
			c.Definition = strip(c.Definition)
		}
		for _, fk := range t.ForeignKeys {
			fk.Definition = strip(fk.Definition)
		}
		for _, c := range t.Columns {
			if c.Default != nil {
				d := strip(*c.Default)
				c.Default = &d
			}
			c.Generated = strip(c.Generated)
			c.Type = strip(c.Type)
		}
	}
	for _, v := range m.Views {
		v.CreateSQL = strip(v.CreateSQL)
	}
	for _, r := range m.Routines {
		r.CreateSQL = strip(r.CreateSQL)
	}
	for _, t := range m.Triggers {
		t.CreateSQL = strip(t.CreateSQL)
	}
}

func readTables(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.Query(ctx, `
		SELECT c.relname, COALESCE(obj_description(c.oid, 'pg_class'), ''),
		       CASE WHEN c.relkind = 'p' THEN 'PARTITION BY ' || pg_get_partkeydef(c.oid) ELSE '' END
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p') AND NOT COALESCE(c.relispartition, false)
		ORDER BY c.relname`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		t := &schemadiff.Table{}
		if err := rows.Scan(&t.Name, &t.Comment, &t.Partitioning); err != nil {
			return err
		}
		m.Tables = append(m.Tables, t)
	}
	return rows.Err()
}

func readColumns(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.Query(ctx, `
		SELECT c.relname, a.attname, a.attnum, format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull, pg_get_expr(d.adbin, d.adrelid), a.attidentity::text,
		       a.attgenerated::text, COALESCE(coll.collname, ''),
		       COALESCE(col_description(a.attrelid, a.attnum), '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_type ty ON ty.oid = a.atttypid
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		LEFT JOIN pg_collation coll ON coll.oid = a.attcollation AND a.attcollation <> ty.typcollation
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p') AND NOT COALESCE(c.relispartition, false)
		  AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY c.relname, a.attnum`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, identity, generated string
		var dflt *string
		col := &schemadiff.Column{}
		if err := rows.Scan(&table, &col.Name, &col.Position, &col.Type, &col.Nullable, &dflt,
			&identity, &generated, &col.Collation, &col.Comment); err != nil {
			return err
		}
		t := m.Table(table)
		if t == nil {
			continue
		}
		switch identity {
		case "a":
			col.Identity, col.AutoIncrement = "ALWAYS", true
		case "d":
			col.Identity, col.AutoIncrement = "BY DEFAULT", true
		}
		switch {
		case generated == "s" && dflt != nil:
			col.Generated, col.GeneratedStored = *dflt, true
		case dflt != nil:
			col.Default, col.DefaultIsExpr = dflt, true
			if strings.HasPrefix(*dflt, "nextval(") {
				col.AutoIncrement = true
			}
		}
		t.Columns = append(t.Columns, col)
	}
	return rows.Err()
}

func readIndexes(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.Query(ctx, `
		SELECT ct.relname, ci.relname, ix.indisprimary, ix.indisunique, am.amname,
		       pg_get_indexdef(ix.indexrelid), COALESCE(obj_description(ci.oid, 'pg_class'), ''),
		       EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = ix.indexrelid AND k.contype IN ('p', 'u')),
		       ARRAY(SELECT CASE WHEN ix.indkey[k] = 0 THEN '' ELSE
		                     (SELECT attname FROM pg_attribute WHERE attrelid = ix.indrelid AND attnum = ix.indkey[k]) END
		             FROM generate_subscripts(ix.indkey, 1) k ORDER BY k),
		       ARRAY(SELECT pg_get_indexdef(ix.indexrelid, k + 1, true)
		             FROM generate_subscripts(ix.indkey, 1) k ORDER BY k),
		       ARRAY(SELECT (ix.indoption[k] & 1) = 1 FROM generate_subscripts(ix.indoption, 1) k ORDER BY k),
		       ix.indpred IS NOT NULL
		FROM pg_index ix
		JOIN pg_class ci ON ci.oid = ix.indexrelid
		JOIN pg_class ct ON ct.oid = ix.indrelid
		JOIN pg_namespace n ON n.oid = ct.relnamespace
		JOIN pg_am am ON am.oid = ci.relam
		WHERE n.nspname = $1 AND ct.relkind IN ('r', 'p') AND NOT COALESCE(ct.relispartition, false)
		ORDER BY ct.relname, ci.relname`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table string
		var cols, exprs []string
		var desc []bool
		ix := &schemadiff.Index{Visible: true}
		if err := rows.Scan(&table, &ix.Name, &ix.Primary, &ix.Unique, &ix.Type, &ix.Definition,
			&ix.Comment, &ix.Constraint, &cols, &exprs, &desc, &ix.Partial); err != nil {
			return err
		}
		t := m.Table(table)
		if t == nil {
			continue
		}
		for i, c := range cols {
			part := schemadiff.IndexPart{Column: c}
			if c == "" && i < len(exprs) {
				part.Expression = exprs[i]
			}
			if i < len(desc) {
				part.Desc = desc[i]
			}
			ix.Parts = append(ix.Parts, part)
		}
		t.Indexes = append(t.Indexes, ix)
	}
	return rows.Err()
}

// fkRule maps pg_constraint's confupdtype/confdeltype codes.
var fkRule = map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}

func readConstraints(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.Query(ctx, `
		SELECT c.relname, k.conname, k.contype::text, pg_get_constraintdef(k.oid),
		       COALESCE(rn.nspname, ''), COALESCE(rc.relname, ''),
		       ARRAY(SELECT a.attname FROM unnest(k.conkey) WITH ORDINALITY u(attnum, ord)
		             JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = u.attnum ORDER BY u.ord),
		       ARRAY(SELECT a.attname FROM unnest(k.confkey) WITH ORDINALITY u(attnum, ord)
		             JOIN pg_attribute a ON a.attrelid = k.confrelid AND a.attnum = u.attnum ORDER BY u.ord),
		       COALESCE(k.confupdtype::text, ''), COALESCE(k.confdeltype::text, ''), k.convalidated
		FROM pg_constraint k
		JOIN pg_class c ON c.oid = k.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_class rc ON rc.oid = k.confrelid
		LEFT JOIN pg_namespace rn ON rn.oid = rc.relnamespace
		WHERE n.nspname = $1 AND k.contype IN ('f', 'c', 'x', 'p', 'u') AND c.relkind IN ('r', 'p')
		  AND NOT COALESCE(c.relispartition, false) AND k.conislocal
		ORDER BY c.relname, k.conname`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, kind, def, refSchema, refTable, onUpdate, onDelete string
		var cols, refCols []string
		var validated bool
		if err := rows.Scan(&table, &name, &kind, &def, &refSchema, &refTable, &cols, &refCols, &onUpdate, &onDelete, &validated); err != nil {
			return err
		}
		t := m.Table(table)
		if t == nil {
			continue
		}
		if kind == "p" || kind == "u" {
			if ix := t.Index(name); ix != nil {
				ix.ConstraintDefinition = def
			}
			continue
		}
		if kind == "f" {
			// pg_get_constraintdef ends with NOT VALID for an unvalidated
			// constraint; that is state, kept apart from the definition.
			t.ForeignKeys = append(t.ForeignKeys, &schemadiff.ForeignKey{Name: name, Columns: cols,
				RefSchema: refSchema, RefTable: refTable, RefColumns: refCols,
				OnUpdate: fkRule[onUpdate], OnDelete: fkRule[onDelete],
				Definition: strings.TrimSuffix(def, " NOT VALID"), NotValid: !validated})
			continue
		}
		t.Checks = append(t.Checks, &schemadiff.Check{Name: name, Definition: def})
	}
	return rows.Err()
}

// readSequences records, for tables with a serial or identity column, the
// next value its sequence hands out (the PostgreSQL analogue of
// AUTO_INCREMENT). Two round trips whatever the table count: one maps each
// table to its sequence (as tableSequence does), one reads them all.
func readSequences(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	if len(m.Tables) == 0 {
		return nil
	}
	names := make([]string, len(m.Tables))
	for i, t := range m.Tables {
		names[i] = t.Name
	}
	rows, err := q.Query(ctx, `
		SELECT t.name, s.seq
		FROM unnest($2::text[]) AS t(name)
		CROSS JOIN LATERAL (
			SELECT pg_get_serial_sequence(quote_ident($1) || '.' || quote_ident(t.name), a.attname) AS seq
			FROM pg_attribute a
			WHERE a.attrelid = (quote_ident($1) || '.' || quote_ident(t.name))::regclass
			  AND a.attnum > 0 AND NOT a.attisdropped
			  AND pg_get_serial_sequence(quote_ident($1) || '.' || quote_ident(t.name), a.attname) IS NOT NULL
			ORDER BY a.attnum LIMIT 1) s`, schema, names)
	if err != nil {
		return err
	}
	var tables, selects []string
	for rows.Next() {
		var table, seq string
		if err := rows.Scan(&table, &seq); err != nil {
			rows.Close()
			return err
		}
		// seq comes from pg_get_serial_sequence, which quotes as needed.
		selects = append(selects, fmt.Sprintf("SELECT %d, CASE WHEN is_called THEN last_value + 1 ELSE last_value END FROM %s", len(tables), seq))
		tables = append(tables, table)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(selects) == 0 {
		return err
	}
	rows, err = q.Query(ctx, strings.Join(selects, " UNION ALL "))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		var next int64
		if err := rows.Scan(&i, &next); err != nil {
			return err
		}
		if next > 0 {
			m.Table(tables[i]).AutoIncrement = uint64(next)
		}
	}
	return rows.Err()
}

// tableSequence returns the qualified sequence owned by the table's serial
// or identity column, or "" when it has none.
func tableSequence(ctx context.Context, q queryer, schema, table string) (string, error) {
	var seq *string
	err := q.QueryRow(ctx, `
		SELECT pg_get_serial_sequence(quote_ident($1) || '.' || quote_ident($2), a.attname)
		FROM pg_attribute a
		WHERE a.attrelid = (quote_ident($1) || '.' || quote_ident($2))::regclass
		  AND a.attnum > 0 AND NOT a.attisdropped
		  AND pg_get_serial_sequence(quote_ident($1) || '.' || quote_ident($2), a.attname) IS NOT NULL
		ORDER BY a.attnum LIMIT 1`, schema, table).Scan(&seq)
	if err == pgx.ErrNoRows || seq == nil {
		return "", nil
	}
	return *seq, err
}

func nextSequenceValue(ctx context.Context, q queryer, schema, table string) (uint64, error) {
	seq, err := tableSequence(ctx, q, schema, table)
	if err != nil || seq == "" {
		return 0, err
	}
	var next int64
	// seq comes from pg_get_serial_sequence, which quotes as needed.
	if err := q.QueryRow(ctx, "SELECT CASE WHEN is_called THEN last_value + 1 ELSE last_value END FROM "+seq).Scan(&next); err != nil {
		return 0, err
	}
	if next < 0 {
		return 0, nil
	}
	return uint64(next), nil
}

func readViews(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.Query(ctx, `
		SELECT c.relname, pg_get_userbyid(c.relowner), pg_get_viewdef(c.oid, true)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'v'
		ORDER BY c.relname`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v := &schemadiff.View{}
		var def string
		if err := rows.Scan(&v.Name, &v.Definer, &def); err != nil {
			return err
		}
		v.CreateSQL = "CREATE OR REPLACE VIEW " + quoteIdent(schema) + "." + quoteIdent(v.Name) + " AS\n" + strings.TrimRight(strings.TrimSpace(def), ";")
		m.Views = append(m.Views, v)
	}
	return rows.Err()
}

func readRoutines(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.Query(ctx, `
		SELECT p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
		       CASE p.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END,
		       pg_get_userbyid(p.proowner), pg_get_functiondef(p.oid)
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.prokind IN ('f', 'p')
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid = p.oid AND d.deptype = 'e')
		ORDER BY 1`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r := &schemadiff.Routine{}
		if err := rows.Scan(&r.Name, &r.Kind, &r.Definer, &r.CreateSQL); err != nil {
			return err
		}
		m.Routines = append(m.Routines, r)
	}
	return rows.Err()
}

func readTriggers(ctx context.Context, q queryer, schema string, m *schemadiff.Model) error {
	rows, err := q.Query(ctx, `
		SELECT t.tgname, c.relname, pg_get_triggerdef(t.oid, true)
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND NOT t.tgisinternal
		ORDER BY c.relname, t.tgname`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		t := &schemadiff.Trigger{}
		if err := rows.Scan(&t.Name, &t.Table, &t.CreateSQL); err != nil {
			return err
		}
		m.Triggers = append(m.Triggers, t)
	}
	return rows.Err()
}

func quoteIdent(name string) string { return driver.QuoteIdentANSI(name) }
