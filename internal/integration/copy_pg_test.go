package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/driver/pg"
)

const pgCopyFixture = `
DROP SCHEMA IF EXISTS cp_src CASCADE; DROP SCHEMA IF EXISTS cp_dst CASCADE;
CREATE SCHEMA cp_src; CREATE SCHEMA cp_dst;
SET search_path = cp_src;
CREATE TYPE mood AS ENUM ('sad','ok','happy');
CREATE TABLE customer (id SERIAL PRIMARY KEY, name VARCHAR(32) NOT NULL, email TEXT, status VARCHAR(32) DEFAULT 'new',
  created TIMESTAMP(3) DEFAULT now(), feeling mood, tags TEXT[], CONSTRAINT ck_name CHECK (length(name) > 0));
COMMENT ON TABLE customer IS 'customers'; COMMENT ON COLUMN customer.email IS 'contact';
CREATE INDEX idx_name ON customer (lower(name));
CREATE TABLE orders (id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  customer_id INT NOT NULL REFERENCES customer(id) ON DELETE CASCADE, amount NUMERIC(12,2) NOT NULL,
  meta JSONB, blobby BYTEA, at TIMESTAMPTZ, total_x2 NUMERIC(13,2) GENERATED ALWAYS AS (amount*2) STORED, uid UUID);
CREATE TABLE empty_t (code CHAR(4) NOT NULL, label TEXT, CONSTRAINT uq_code UNIQUE (code));
CREATE TABLE nokey (a INT, b TEXT);
CREATE TABLE strkey (k TEXT PRIMARY KEY, v INT);
CREATE VIEW v_big AS SELECT id, amount FROM orders WHERE amount > 100;
CREATE FUNCTION trg_fn() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN NEW.status := COALESCE(NEW.status,'new'); RETURN NEW; END$$;
CREATE TRIGGER trg_cust BEFORE INSERT ON customer FOR EACH ROW EXECUTE FUNCTION trg_fn();
INSERT INTO customer (name,email,status,created,feeling,tags) VALUES
  ('Alice','a@x','vip','2026-01-02 03:04:05.678','happy','{a,b}'),('Bob',NULL,'new','2026-02-02 00:00:00.001',NULL,NULL),
  ('Héloïse','h@x','','2026-03-03',NULL,'{}'),('Zed','',NULL,NULL,'sad','{"with space",NULL}');
INSERT INTO orders (customer_id,amount,meta,blobby,at,uid) VALUES
  (1,150.50,'{"a": [1, 2]}','\x00ff10','2026-01-01 10:00:00.123456+02','a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'),
  (2,20.00,NULL,'',NULL,NULL),(1,999.99,'"x"',NULL,'infinity',NULL);
INSERT INTO nokey VALUES (1,'x'),(1,'x');
INSERT INTO strkey VALUES ('a',1),('B',2),('c',3),('',0);
SET search_path = cp_dst;
CREATE TYPE mood AS ENUM ('sad','ok','happy');
CREATE TABLE customer (id SERIAL PRIMARY KEY, name VARCHAR(64) NOT NULL, email TEXT, legacy INT, feeling mood, tags TEXT[],
  CONSTRAINT ck_name CHECK (length(name) > 0));
CREATE INDEX idx_merchant ON customer (email);
CREATE TABLE strkey (k TEXT PRIMARY KEY, v INT);
CREATE TABLE dst_only (x INT PRIMARY KEY);
INSERT INTO customer (id,name,email,legacy) VALUES (2,'Bobby',NULL,7),(50,'DevOnly','d@x',1);
INSERT INTO strkey VALUES ('A',9),('b',2),('z',5);
`

// pgRowsDigest renders a table's rows column by column (independent of
// physical column order) as text.
func pgRowsDigest(t *testing.T, conn *pgx.Conn, schema, table, cols string) string {
	t.Helper()
	var out string
	row := "concat_ws('|', " + cols + ")"
	q := fmt.Sprintf("SELECT COALESCE(string_agg(%s, E'\\n' ORDER BY %s COLLATE \"C\"), '') FROM %s.%s", row, row, schema, table)
	if err := conn.QueryRow(context.Background(), q).Scan(&out); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

func TestDockerCopyPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping docker integration tests in short mode")
	}
	if !isPortOpen("15432") {
		t.Skip("pg container not available on port 15432")
	}
	bin := buildBinary(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, "postgres://test:test@127.0.0.1:15432/testdb?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, pgCopyFixture); err != nil {
		t.Fatalf("seed: %v", err)
	}

	xdg := t.TempDir()
	dir := filepath.Join(xdg, "agent-sql")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"connections":{
		"pgw":{"driver":"pg","host":"127.0.0.1","port":15432,"database":"testdb","credential":"rw","options":{"sslmode":"disable"}},
		"gone":{"driver":"pg","host":"127.0.0.1","port":15432,"database":"no_such_db","credential":"rw","options":{"sslmode":"disable"}},
		"plain":{"driver":"pg","host":"127.0.0.1","port":15432,"database":"testdb","credential":"plain","options":{"sslmode":"disable"}}
	},"settings":{}}`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{
		"rw":{"username":"test","password":"test","writePermission":true},
		"plain":{"username":"cp_plain","password":"plain","writePermission":true}}`), 0o600)
	e := &copyEnv{t: t, bin: bin, xdg: xdg}

	base := []string{"--from", "pgw", "--to", "pgw", "--schema", "cp_src", "--to-schema", "cp_dst"}
	all := "tables,views,routines,triggers"
	digests := map[string]string{
		"customer": "id, name, quote_nullable(email), quote_nullable(status), quote_nullable(created), quote_nullable(feeling), quote_nullable(tags)",
		"orders":   "id, customer_id, amount, quote_nullable(meta), quote_nullable(blobby), quote_nullable(at), total_x2, quote_nullable(uid)",
		"strkey":   "quote_literal(k), v",
	}
	sameRows := func(t *testing.T) {
		t.Helper()
		for table, cols := range digests {
			if a, b := pgRowsDigest(t, conn, "cp_src", table, cols), pgRowsDigest(t, conn, "cp_dst", table, cols); a != b {
				t.Errorf("%s differs:\n%s\n---\n%s", table, a, b)
			}
		}
	}

	t.Run("schema databases falls back to postgres", func(t *testing.T) {
		// The alias's database is gone: listing still works, through the
		// driver's default database, while other commands still fail.
		stdout, stderr, ok := e.run("schema", "databases", "-c", "gone")
		if !ok {
			t.Fatalf("schema databases: %s", stderr)
		}
		found := false
		for _, r := range records(t, stdout) {
			found = found || r["name"] == "testdb"
		}
		if !found {
			t.Errorf("testdb not listed:\n%s", stdout)
		}
		if _, stderr, ok := e.run("schema", "tables", "-c", "gone"); ok || !strings.Contains(stderr, "no_such_db") {
			t.Errorf("schema tables on a missing database: ok=%v %s", ok, stderr)
		}
	})

	t.Run("schema diff", func(t *testing.T) {
		stdout, stderr, ok := e.run(append([]string{"schema", "diff", "--objects", all}, base...)...)
		if !ok {
			t.Fatalf("diff: %s", stderr)
		}
		for _, r := range records(t, stdout) {
			if r["object"] == "column" && (r["name"] == "id" || r["name"] == "feeling") {
				t.Errorf("schema qualifier noise: %v", r)
			}
		}
		sqlOut, _, _ := e.run(append([]string{"schema", "diff", "--objects", all, "--format", "sql"}, base...)...)
		if !strings.Contains(sqlOut, `CREATE TABLE "cp_dst"."orders"`) || !strings.Contains(sqlOut, `"cp_dst".trg_fn()`) {
			t.Errorf("sql:\n%s", sqlOut)
		}
	})

	t.Run("sync structure and data", func(t *testing.T) {
		args := append(append([]string{"copy"}, base...), "--structure", "sync", "--data", "sync",
			"--exclude-tables", "nokey", "--allow-destructive", "--objects", all)
		if _, stderr, ok := e.run(args...); !ok {
			t.Fatalf("copy: %s", stderr)
		}
		sameRows(t)
		stdout, _, _ := e.run(append([]string{"schema", "diff", "--objects", all, "--exclude-tables", "nokey"}, base...)...)
		if r := last(t, stdout); r["different"].(float64) != 0 || r["only_in_source"].(float64) != 0 {
			t.Errorf("rediff: %s", stdout)
		}
		stdout, _, _ = e.run(args...)
		if r := last(t, stdout); r["inserted"].(float64)+r["updated"].(float64)+r["deleted"].(float64) != 0 {
			t.Errorf("second sync not a no-op: %v", r)
		}
		var next int64
		_ = conn.QueryRow(ctx, "SELECT nextval('cp_dst.customer_id_seq')").Scan(&next)
		if next != 5 {
			t.Errorf("serial sequence not realigned: next = %d", next)
		}
	})

	t.Run("secondary unique index never redirects a write", func(t *testing.T) {
		// Target-only row 900 holds the email source row 2 needs: sync
		// deletes it first and inserts row 2; upsert, which keeps it, fails
		// the table rather than writing over it.
		for _, stmt := range []string{
			"CREATE TABLE cp_src.uq (id INT PRIMARY KEY, email TEXT NOT NULL UNIQUE, v INT)",
			"CREATE TABLE cp_dst.uq (id INT PRIMARY KEY, email TEXT NOT NULL UNIQUE, v INT)",
			"INSERT INTO cp_src.uq VALUES (1,'e1',1),(2,'a',2),(3,'e3',3),(4,'e4',4),(5,'e5',5)",
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		defer func() { _, _ = conn.Exec(ctx, "DROP TABLE cp_src.uq; DROP TABLE cp_dst.uq") }()
		reset := func() {
			if _, err := conn.Exec(ctx, "DELETE FROM cp_dst.uq; INSERT INTO cp_dst.uq VALUES (900,'a',99)"); err != nil {
				t.Fatal(err)
			}
		}
		cols := "id, email, v"
		reset()
		args := append(append([]string{"copy"}, base...), "--structure", "none", "--tables", "uq", "--batch-rows", "1", "--allow-destructive")
		if _, stderr, ok := e.run(append(args, "--data", "sync")...); !ok {
			t.Fatalf("sync: %s", stderr)
		}
		if a, b := pgRowsDigest(t, conn, "cp_src", "uq", cols), pgRowsDigest(t, conn, "cp_dst", "uq", cols); a != b {
			t.Errorf("uq differs:\n%s\n---\n%s", a, b)
		}
		reset()
		if _, _, ok := e.run(append(args, "--data", "upsert")...); ok {
			t.Error("upsert colliding on a secondary unique index should fail")
		}
		if got := pgRowsDigest(t, conn, "cp_dst", "uq", cols); got != "900|a|99" {
			t.Errorf("target after a failed upsert: %q", got)
		}
	})

	t.Run("foreign key rules and attributes", func(t *testing.T) {
		fkDiffers := func(t *testing.T, change string) {
			t.Helper()
			stdout, stderr, ok := e.run(append([]string{"schema", "diff", "--tables", "orders"}, base...)...)
			if !ok {
				t.Fatalf("diff: %s", stderr)
			}
			for _, r := range records(t, stdout) {
				if r["object"] == "foreign_key" && r["name"] == "orders_customer_id_fkey" {
					if ch, _ := r["changes"].(map[string]any); ch[change] == nil {
						t.Errorf("fk changes = %v, want %q", r["changes"], change)
					}
					return
				}
			}
			t.Errorf("foreign key difference not reported (%s):\n%s", change, stdout)
		}
		setSourceFK := func(t *testing.T, tail string) {
			t.Helper()
			for _, stmt := range []string{
				"ALTER TABLE cp_src.orders DROP CONSTRAINT orders_customer_id_fkey",
				"ALTER TABLE cp_src.orders ADD CONSTRAINT orders_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES cp_src.customer(id) " + tail,
			} {
				if _, err := conn.Exec(ctx, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
		}
		syncAndRediff := func(t *testing.T) {
			t.Helper()
			if _, stderr, ok := e.run(append(append([]string{"copy"}, base...), "--structure", "sync", "--data", "none",
				"--tables", "orders", "--allow-destructive")...); !ok {
				t.Fatalf("copy: %s", stderr)
			}
			stdout, _, _ := e.run(append([]string{"schema", "diff", "--tables", "orders"}, base...)...)
			if r := last(t, stdout); r["different"].(float64) != 0 {
				t.Errorf("rediff: %s", stdout)
			}
		}
		// PostgreSQL's RESTRICT is not NO ACTION: it cannot wait for the
		// end of the statement or be deferred.
		setSourceFK(t, "ON DELETE CASCADE ON UPDATE RESTRICT")
		fkDiffers(t, "definition")
		syncAndRediff(t)
		// Attributes beyond the rules come from the server's definition.
		setSourceFK(t, "MATCH FULL ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED")
		fkDiffers(t, "definition")
		syncAndRediff(t)
		var deferred bool
		var match string
		if err := conn.QueryRow(ctx, `SELECT condeferred, confmatchtype::text FROM pg_constraint
			WHERE conname = 'orders_customer_id_fkey' AND connamespace = 'cp_dst'::regnamespace`).Scan(&deferred, &match); err != nil {
			t.Fatal(err)
		}
		if !deferred || match != "f" {
			t.Errorf("target fk deferred=%v match=%s, want deferred MATCH FULL", deferred, match)
		}
		setSourceFK(t, "ON DELETE CASCADE")
		syncAndRediff(t)
		// Validation state is not structure: a NOT VALID source leaves a
		// validated target alone.
		setSourceFK(t, "ON DELETE CASCADE NOT VALID")
		stdout, _, _ := e.run(append([]string{"schema", "diff", "--tables", "orders"}, base...)...)
		if r := last(t, stdout); r["different"].(float64) != 0 {
			t.Errorf("NOT VALID reported as a difference: %s", stdout)
		}
		setSourceFK(t, "ON DELETE CASCADE")
	})

	t.Run("recreate with views and triggers", func(t *testing.T) {
		args := append(append([]string{"copy"}, base...), "--structure", "recreate", "--data", "replace",
			"--allow-destructive", "--objects", all)
		if _, stderr, ok := e.run(args...); !ok {
			t.Fatalf("recreate: %s", stderr)
		}
		sameRows(t)
		stdout, _, _ := e.run(append([]string{"schema", "diff", "--objects", all}, base...)...)
		if r := last(t, stdout); r["different"].(float64) != 0 || r["only_in_source"].(float64) != 0 {
			t.Errorf("rediff after recreate: %s", stdout)
		}
	})

	t.Run("--where resolves unqualified names in each schema", func(t *testing.T) {
		where := "orders=customer_id IN (SELECT id FROM customer WHERE feeling = 'happy'::mood)"
		args := append([]string{"copy", "--structure", "none", "--data", "sync", "--tables", "orders", "--where", where, "--allow-destructive"}, base...)
		if _, stderr, ok := e.run(args...); !ok {
			t.Fatalf("sync with --where: %s", stderr)
		}
		sameRows(t)
	})

	t.Run("non-superuser keeps foreign key checks on", func(t *testing.T) {
		for _, stmt := range []string{
			"DROP OWNED BY cp_plain", // fails harmlessly when the role is new
		} {
			_, _ = conn.Exec(ctx, stmt)
		}
		for _, stmt := range []string{
			"DROP ROLE IF EXISTS cp_plain",
			"CREATE ROLE cp_plain LOGIN PASSWORD 'plain'",
			"GRANT USAGE ON SCHEMA cp_src TO cp_plain",
			"GRANT SELECT ON ALL TABLES IN SCHEMA cp_src TO cp_plain",
			"GRANT SELECT ON ALL SEQUENCES IN SCHEMA cp_src TO cp_plain",
			"GRANT ALL ON SCHEMA cp_dst TO cp_plain",
			"GRANT ALL ON ALL TABLES IN SCHEMA cp_dst TO cp_plain",
			"GRANT ALL ON ALL SEQUENCES IN SCHEMA cp_dst TO cp_plain",
			// Drop a parent row (and its child) on the source, so sync must
			// delete the target's child before its parent.
			"DELETE FROM cp_src.orders WHERE customer_id = 2",
			"DELETE FROM cp_src.customer WHERE id = 2",
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		args := []string{"copy", "--from", "plain", "--to", "plain", "--schema", "cp_src", "--to-schema", "cp_dst",
			"--structure", "none", "--data", "sync", "--tables", "customer,orders,strkey", "--allow-destructive"}
		stdout, stderr, ok := e.run(args...)
		if !ok {
			t.Fatalf("sync as non-superuser: %s", stderr)
		}
		if r := last(t, stdout); r["deleted"].(float64) != 2 {
			t.Errorf("receipt = %v", r)
		}
		sameRows(t)
		// A target child points at a target-only parent the source never
		// had: the parent can only go once the child is re-pointed. (The
		// foreign key must not cascade, or the delete takes the child along.)
		for _, stmt := range []string{
			"ALTER TABLE cp_dst.orders DROP CONSTRAINT orders_customer_id_fkey",
			"ALTER TABLE cp_dst.orders ADD CONSTRAINT orders_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES cp_dst.customer(id)",
			"INSERT INTO cp_dst.customer (id, name) VALUES (70, 'Orphaned')",
			"UPDATE cp_dst.orders SET customer_id = 70 WHERE id = (SELECT min(id) FROM cp_dst.orders)",
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		stdout, stderr, ok = e.run(args...)
		if !ok {
			t.Fatalf("sync re-pointing a child as non-superuser: %s", stderr)
		}
		if r := last(t, stdout); r["deleted"].(float64) != 1 || r["updated"].(float64) != 1 {
			t.Errorf("re-point receipt = %v", r)
		}
		sameRows(t)
		for _, stmt := range []string{
			"ALTER TABLE cp_dst.orders DROP CONSTRAINT orders_customer_id_fkey",
			"ALTER TABLE cp_dst.orders ADD CONSTRAINT orders_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES cp_dst.customer(id) ON DELETE CASCADE",
		} {
			if _, err := conn.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		args[12] = "replace"
		if _, stderr, ok := e.run(args...); !ok {
			t.Fatalf("replace as non-superuser: %s", stderr)
		}
		sameRows(t)
		_, _ = conn.Exec(ctx, "DROP OWNED BY cp_plain")
		_, _ = conn.Exec(ctx, "DROP ROLE IF EXISTS cp_plain")
	})

	t.Run("dry-run sync counts deletes when the key is new", func(t *testing.T) {
		// The target lacks the key column the structure phase adds. A --where
		// naming it is not tried on the target: a failed read would abort the
		// snapshot every later table reads through.
		if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS cp_r_src CASCADE; DROP SCHEMA IF EXISTS cp_r_dst CASCADE;
			CREATE SCHEMA cp_r_src; CREATE SCHEMA cp_r_dst;
			CREATE TABLE cp_r_src.k (id INT PRIMARY KEY, v INT); INSERT INTO cp_r_src.k VALUES (1,1),(2,2);
			CREATE TABLE cp_r_src.z (id INT PRIMARY KEY); INSERT INTO cp_r_src.z VALUES (1);
			CREATE TABLE cp_r_dst.k (v INT); INSERT INTO cp_r_dst.k VALUES (5),(6),(7);
			CREATE TABLE cp_r_dst.z (id INT PRIMARY KEY);`); err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = conn.Exec(ctx, "DROP SCHEMA cp_r_src CASCADE; DROP SCHEMA cp_r_dst CASCADE") }()
		for _, c := range []struct {
			where             []string
			deleted, inserted float64
		}{
			{nil, 3, 2},
			{[]string{"--where", "k=v > 5"}, 2, 0},
			{[]string{"--where", "k=id > 1"}, 3, 1},
		} {
			args := append([]string{"copy", "--from", "pgw", "--to", "pgw", "--schema", "cp_r_src", "--to-schema", "cp_r_dst",
				"--structure", "sync", "--data", "sync", "--dry-run"}, c.where...)
			stdout, stderr, ok := e.run(args...)
			if !ok {
				t.Fatalf("dry run %v: %s", c.where, stderr)
			}
			for _, r := range records(t, stdout) {
				switch r["table"] {
				case "k":
					if r["deleted"].(float64) != c.deleted || r["inserted"].(float64) != c.inserted {
						t.Errorf("dry run %v: %v", c.where, r)
					}
				case "z":
					if r["inserted"].(float64) != 1 {
						t.Errorf("dry run %v, later table: %v", c.where, r)
					}
				}
			}
		}
	})
}

// TestDockerPostgresSessionMarker checks the same-server probe copy uses when
// ServerIdentity differs between routes: a marker held by one session is
// seen from another session of the same database, not from another database.
func TestDockerPostgresSessionMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping docker integration tests in short mode")
	}
	if !isPortOpen("15432") {
		t.Skip("pg container not available on port 15432")
	}
	ctx := context.Background()
	open := func(url string) driver.SessionMarker {
		t.Helper()
		c, err := pg.ConnectURL(ctx, url, true)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c.(driver.SessionMarker)
	}
	src := open("postgres://test:test@127.0.0.1:15432/testdb?sslmode=disable")
	same := open("postgres://test:test@localhost:15432/testdb?sslmode=disable")
	other := open("postgres://test:test@127.0.0.1:15432/postgres?sslmode=disable")

	release, err := src.HoldMarker(ctx, 424242)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := same.MarkerHeld(ctx, 424242); err != nil || !held {
		t.Errorf("same database: held=%v err=%v", held, err)
	}
	if held, err := other.MarkerHeld(ctx, 424242); err != nil || held {
		t.Errorf("other database: held=%v err=%v", held, err)
	}
	release()
	if held, err := same.MarkerHeld(ctx, 424242); err != nil || held {
		t.Errorf("after release: held=%v err=%v", held, err)
	}
}
