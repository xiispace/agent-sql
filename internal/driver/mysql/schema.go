package mysql

import (
	"context"
	"strings"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/errors"
)

// systemDatabases are hidden from `schema databases` unless --include-system.
var systemDatabases = map[string]bool{
	"information_schema": true, "performance_schema": true, "mysql": true, "sys": true,
}

// errNoDatabase is returned by whole-database schema commands on a connection
// with no default database and no db.table qualifier to scope to.
func errNoDatabase() error {
	return errors.New("No database selected.", errors.FixableByAgent).WithHint(errors.HintNoDatabase)
}

// scope returns the database whole-database schema commands read: the
// connection's database (-D or stored/URL database).
func (c *mysqlConn) scope() (string, error) {
	if c.database == "" {
		return "", errNoDatabase()
	}
	return c.database, nil
}

// splitTable splits an optionally db-qualified table name into (database,
// table), defaulting the database to the connection's; an empty name scopes
// to the whole database (table ""). An empty database is
// an error: MySQL schema queries are always addressed by name, never by
// DATABASE().
func (c *mysqlConn) splitTable(name string) (string, string, error) {
	db, table := driver.SplitSchemaTable(name, c.database)
	if db == "" {
		return "", "", errNoDatabase()
	}
	return db, table, nil
}

// quoteQualified quotes `db`.`table` (or just `table`) part by part.
func (c *mysqlConn) quoteQualified(db, table string) string {
	if db == "" {
		return quoteIdent(table)
	}
	return qualify(db, table)
}

func (c *mysqlConn) GetTables(ctx context.Context, includeSystem bool) ([]driver.TableInfo, error) {
	db, err := c.scope()
	if err != nil {
		return nil, err
	}
	query := `
		SELECT table_name, table_type
		FROM information_schema.tables
		WHERE table_schema = ?
		  AND table_type IN ('BASE TABLE', 'VIEW')
		ORDER BY table_name`

	rows, err := c.db.QueryContext(ctx, query, db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tables []driver.TableInfo
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			return nil, err
		}
		t := "table"
		if typ == "VIEW" {
			t = "view"
		}
		tables = append(tables, driver.TableInfo{Name: name, Type: t})
	}
	return tables, rows.Err()
}

func (c *mysqlConn) DescribeTable(ctx context.Context, name string) ([]driver.ColumnInfo, error) {
	db, table, err := c.splitTable(name)
	if err != nil {
		return nil, err
	}
	query := `
		SELECT
			c.COLUMN_NAME,
			c.COLUMN_TYPE,
			c.IS_NULLABLE,
			c.COLUMN_DEFAULT,
			c.COLUMN_KEY
		FROM information_schema.columns c
		WHERE c.TABLE_SCHEMA = ?
		  AND c.TABLE_NAME = ?
		ORDER BY c.ORDINAL_POSITION`

	rows, err := c.db.QueryContext(ctx, query, db, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var columns []driver.ColumnInfo
	for rows.Next() {
		var name, colType, nullable, colKey string
		var dflt *string
		if err := rows.Scan(&name, &colType, &nullable, &dflt, &colKey); err != nil {
			return nil, err
		}
		col := driver.ColumnInfo{
			Name:       name,
			Type:       colType,
			Nullable:   nullable == "YES",
			PrimaryKey: colKey == "PRI",
		}
		if dflt != nil {
			col.DefaultValue = *dflt
		}
		columns = append(columns, col)
	}
	return columns, rows.Err()
}

func (c *mysqlConn) GetIndexes(ctx context.Context, name string) ([]driver.IndexInfo, error) {
	db, table, err := c.splitTable(name)
	if err != nil {
		return nil, err
	}
	query := `
		SELECT
			INDEX_NAME,
			TABLE_NAME,
			GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX) AS idx_columns,
			NOT NON_UNIQUE AS is_unique
		FROM information_schema.statistics
		WHERE TABLE_SCHEMA = ?`

	args := []any{db}
	if table != "" {
		query += " AND TABLE_NAME = ?"
		args = append(args, table)
	}
	query += `
		GROUP BY TABLE_NAME, INDEX_NAME, NON_UNIQUE
		ORDER BY TABLE_NAME, INDEX_NAME`

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var indexes []driver.IndexInfo
	for rows.Next() {
		var name, tableName, cols string
		var unique bool
		if err := rows.Scan(&name, &tableName, &cols, &unique); err != nil {
			return nil, err
		}
		indexes = append(indexes, driver.IndexInfo{
			Name:    name,
			Table:   tableName,
			Columns: strings.Split(cols, ","),
			Unique:  unique,
		})
	}
	return indexes, rows.Err()
}

func (c *mysqlConn) GetConstraints(ctx context.Context, name string) ([]driver.ConstraintInfo, error) {
	db, table, err := c.splitTable(name)
	if err != nil {
		return nil, err
	}
	query := `
		SELECT
			tc.CONSTRAINT_NAME,
			tc.TABLE_NAME,
			tc.CONSTRAINT_TYPE,
			GROUP_CONCAT(kcu.COLUMN_NAME ORDER BY kcu.ORDINAL_POSITION) AS cols,
			kcu.REFERENCED_TABLE_NAME,
			GROUP_CONCAT(kcu.REFERENCED_COLUMN_NAME ORDER BY kcu.ORDINAL_POSITION) AS ref_cols
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
		  AND kcu.TABLE_SCHEMA = tc.TABLE_SCHEMA
		  AND kcu.TABLE_NAME = tc.TABLE_NAME
		WHERE tc.TABLE_SCHEMA = ?`

	args := []any{db}
	if table != "" {
		query += " AND tc.TABLE_NAME = ?"
		args = append(args, table)
	}
	query += `
		GROUP BY tc.CONSTRAINT_NAME, tc.TABLE_NAME, tc.CONSTRAINT_TYPE,
		         kcu.REFERENCED_TABLE_NAME
		ORDER BY tc.TABLE_NAME, tc.CONSTRAINT_NAME`

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var constraints []driver.ConstraintInfo
	for rows.Next() {
		var name, tableName, cType, cols string
		var refTable, refCols *string
		if err := rows.Scan(&name, &tableName, &cType, &cols, &refTable, &refCols); err != nil {
			return nil, err
		}
		ct := driver.MapConstraintType(cType)
		if ct == "" {
			ct = driver.ConstraintCheck
		}
		ci := driver.ConstraintInfo{
			Name:    name,
			Table:   tableName,
			Type:    ct,
			Columns: strings.Split(cols, ","),
		}
		if cType == "FOREIGN KEY" && refTable != nil {
			ci.ReferencedTable = *refTable
			if refCols != nil {
				ci.ReferencedColumns = strings.Split(*refCols, ",")
			}
		}
		constraints = append(constraints, ci)
	}
	return constraints, rows.Err()
}

func (c *mysqlConn) GetDDL(ctx context.Context, name string) (string, error) {
	db, table := driver.SplitSchemaTable(name, "")
	var tblName, ddl string
	err := c.db.QueryRowContext(ctx, "SHOW CREATE TABLE "+c.quoteQualified(db, table)).Scan(&tblName, &ddl)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(strings.TrimSpace(ddl), ";") {
		ddl = ddl + ";"
	}
	return ddl, nil
}

func (c *mysqlConn) SearchSchema(ctx context.Context, pattern string) (*driver.SearchResult, error) {
	db, err := c.scope()
	if err != nil {
		return nil, err
	}
	likePattern := "%" + pattern + "%"

	tableRows, err := c.db.QueryContext(ctx,
		`SELECT TABLE_NAME
		 FROM information_schema.tables
		 WHERE TABLE_SCHEMA = ?
		   AND TABLE_NAME LIKE ?
		 ORDER BY TABLE_NAME`, db, likePattern)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tableRows.Close() }()

	var tables []driver.TableInfo
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, driver.TableInfo{Name: name})
	}

	colRows, err := c.db.QueryContext(ctx,
		`SELECT TABLE_NAME, COLUMN_NAME
		 FROM information_schema.columns
		 WHERE TABLE_SCHEMA = ?
		   AND COLUMN_NAME LIKE ?
		 ORDER BY TABLE_NAME, COLUMN_NAME`, db, likePattern)
	if err != nil {
		return nil, err
	}
	defer func() { _ = colRows.Close() }()

	var columns []driver.ColumnMatch
	for colRows.Next() {
		var tbl, col string
		if err := colRows.Scan(&tbl, &col); err != nil {
			return nil, err
		}
		columns = append(columns, driver.ColumnMatch{Table: tbl, Column: col})
	}

	return &driver.SearchResult{Tables: tables, Columns: columns}, nil
}

// ListDatabases reads information_schema.schemata, which MySQL filters by the
// credential's privileges: a restricted account sees only its databases.
func (c *mysqlConn) ListDatabases(ctx context.Context, includeSystem bool) ([]driver.DatabaseInfo, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME
		FROM information_schema.schemata
		ORDER BY SCHEMA_NAME`)
	if err != nil {
		return nil, classifyError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []driver.DatabaseInfo
	for rows.Next() {
		var d driver.DatabaseInfo
		if err := rows.Scan(&d.Name, &d.Charset, &d.Collation); err != nil {
			return nil, err
		}
		if !includeSystem && systemDatabases[strings.ToLower(d.Name)] {
			continue
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
