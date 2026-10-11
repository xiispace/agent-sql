package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/shhac/agent-sql/internal/driver"
	mysqldrv "github.com/shhac/agent-sql/internal/driver/mysql"
)

// copyServer is one MySQL-family container from docker-compose.test.yml.
type copyServer struct {
	name   string
	driver string
	port   string
}

var copyServers = []copyServer{
	{"mysql", "mysql", "13306"},
	{"mariadb", "mariadb", "13307"},
}

// copyFixture builds a source schema with the awkward cases copy must
// round-trip, and a drifted target schema.
const copyFixture = `
DROP DATABASE IF EXISTS cp_src; DROP DATABASE IF EXISTS cp_dst; DROP DATABASE IF EXISTS cp_other;
CREATE DATABASE cp_src; CREATE DATABASE cp_dst; CREATE DATABASE cp_other;
USE cp_src;
CREATE TABLE customer (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(32) NOT NULL, email VARCHAR(64),
  status VARCHAR(32) DEFAULT 'new', created DATETIME(3) DEFAULT CURRENT_TIMESTAMP(3), KEY idx_name (name));
CREATE TABLE orders (id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY, customer_id INT NOT NULL,
  amount DECIMAL(12,2) NOT NULL, meta TEXT, blobby VARBINARY(16), zero_dt DATETIME,
  total_x2 DECIMAL(13,2) AS (amount*2) STORED,
  CONSTRAINT fk_cust FOREIGN KEY (customer_id) REFERENCES customer(id) ON DELETE CASCADE);
CREATE TABLE empty_t (code CHAR(4) NOT NULL, label TEXT, UNIQUE KEY uq_code (code));
CREATE TABLE nokey (a INT, b TEXT);
CREATE TABLE strkey (k VARCHAR(10) COLLATE utf8mb4_general_ci PRIMARY KEY, v INT);
SET SESSION sql_mode = 'NO_AUTO_VALUE_ON_ZERO';
INSERT INTO customer (id,name,email,status,created) VALUES
  (0,'zero',NULL,NULL,NULL),
  (1,'Alice','a@x','vip','2026-01-02 03:04:05.678'),(2,'Bob',NULL,'new','2026-02-02 00:00:00.001'),
  (3,'Héloïse','h@x','','2026-03-03 00:00:00'),(4,'Zed','',NULL,NULL);
INSERT INTO orders (customer_id,amount,meta,blobby,zero_dt) VALUES
  (1,150.50,'{"a": [1, 2]}',0x00FF10,'0000-00-00 00:00:00'),(2,20.00,NULL,'',NULL),(1,999.99,'"x"',NULL,'2026-01-01 00:00:00');
INSERT INTO nokey VALUES (1,'x'),(1,'x');
INSERT INTO strkey VALUES ('a',1),('B',2),('c',3),('',0);
USE cp_dst;
CREATE TABLE customer (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(64) NOT NULL, email VARCHAR(64),
  legacy INT, KEY idx_merchant (email));
CREATE TABLE strkey (k VARCHAR(10) COLLATE utf8mb4_general_ci PRIMARY KEY, v INT);
CREATE TABLE dst_only (x INT PRIMARY KEY);
INSERT INTO customer (id,name,email,legacy) VALUES (2,'Bobby',NULL,7),(50,'DevOnly','d@x',1);
INSERT INTO strkey VALUES ('A',9),('b',2),('z',5);
`

// copyEnv is an isolated agent-sql config: stored aliases for the server, a
// credential with writePermission and one without, as plain-file entries so
// nothing touches the system Keychain.
type copyEnv struct {
	t   *testing.T
	bin string
	xdg string
	db  *sql.DB
}

func newCopyEnv(t *testing.T, bin string, s copyServer) *copyEnv {
	t.Helper()
	xdg := t.TempDir()
	dir := filepath.Join(xdg, "agent-sql")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"connections":{
		"w":{"driver":%q,"host":"127.0.0.1","port":%s,"credential":"rw"},
		"ro":{"driver":%q,"host":"127.0.0.1","port":%s,"credential":"ro"},
		"restricted":{"driver":%q,"host":"127.0.0.1","port":%s,"credential":"limited"}
	},"settings":{}}`, s.driver, s.port, s.driver, s.port, s.driver, s.port)
	creds := `{"rw":{"username":"root","password":"test","writePermission":true},
		"ro":{"username":"root","password":"test","writePermission":false},
		"limited":{"username":"cp_limited","password":"limited","writePermission":false}}`
	for name, body := range map[string]string{"config.json": cfg, "credentials.json": creds} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("mysql", "root:test@tcp(127.0.0.1:"+s.port+")/?multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &copyEnv{t: t, bin: bin, xdg: xdg, db: db}
}

func (e *copyEnv) seed() {
	e.t.Helper()
	if _, err := e.db.Exec(copyFixture); err != nil {
		e.t.Fatalf("seed: %v", err)
	}
}

func (e *copyEnv) exec(q string) {
	e.t.Helper()
	if _, err := e.db.Exec(q); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

// run invokes the CLI and returns stdout, stderr and whether it exited 0.
func (e *copyEnv) run(args ...string) (string, string, bool) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+e.xdg, "AGENT_SQL_CONNECTION=")
	err := cmd.Run()
	return outBuf.String(), errBuf.String(), err == nil
}

// records parses NDJSON output lines.
func records(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range nonEmptyLines(stdout) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func last(t *testing.T, stdout string) map[string]any {
	t.Helper()
	recs := records(t, stdout)
	if len(recs) == 0 {
		t.Fatalf("no output")
	}
	return recs[len(recs)-1]
}

func errorOf(t *testing.T, stderr string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(lastLine(stderr))), &m); err != nil {
		t.Fatalf("stderr is not a JSON error: %q", stderr)
	}
	return m
}

func lastLine(s string) string {
	lines := nonEmptyLines(s)
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// dump renders a table's rows over the text protocol, NULL distinct from
// the empty string, so two schemas can be compared byte for byte.
func (e *copyEnv) dump(schema, table string) string {
	e.t.Helper()
	rows, err := e.db.Query(fmt.Sprintf("SELECT * FROM `%s`.`%s` ORDER BY 1", schema, table))
	if err != nil {
		e.t.Fatalf("dump %s.%s: %v", schema, table, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var b strings.Builder
	for rows.Next() {
		vals := make([]*[]byte, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			vals[i] = new([]byte)
			ptrs[i] = vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			e.t.Fatal(err)
		}
		for _, v := range vals {
			if *v == nil {
				b.WriteString("NULL|")
			} else {
				fmt.Fprintf(&b, "%q|", *v)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (e *copyEnv) assertSameRows(tables ...string) {
	e.t.Helper()
	for _, tbl := range tables {
		if src, dst := e.dump("cp_src", tbl), e.dump("cp_dst", tbl); src != dst {
			e.t.Errorf("%s differs:\nsource:\n%s\ntarget:\n%s", tbl, src, dst)
		}
	}
}

func TestDockerCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping docker integration tests in short mode")
	}
	bin := buildBinary(t)
	for _, s := range copyServers {
		t.Run(s.name, func(t *testing.T) {
			if !isPortOpen(s.port) {
				t.Skipf("%s container not available on port %s", s.name, s.port)
			}
			e := newCopyEnv(t, bin, s)
			e.seed()
			base := []string{"--from", "w", "--to", "w", "--schema", "cp_src", "--to-schema", "cp_dst"}
			copyArgs := func(extra ...string) []string { return append(append([]string{"copy"}, base...), extra...) }
			diffArgs := func(extra ...string) []string {
				return append(append([]string{"schema", "diff"}, base...), extra...)
			}

			t.Run("database addressing", func(t *testing.T) {
				_, stderr, ok := e.run("schema", "tables", "-c", "w")
				if ok || !strings.Contains(errorOf(t, stderr)["hint"].(string), "schema databases") {
					t.Errorf("no database: ok=%v stderr=%s", ok, stderr)
				}
				stdout, _, ok := e.run("schema", "tables", "-c", "w", "-D", "cp_src")
				if !ok || len(records(t, stdout)) != 5 {
					t.Errorf("-D tables: %s", stdout)
				}
				stdout, _, ok = e.run("schema", "describe", "cp_src.customer", "-c", "w")
				if !ok || !strings.Contains(stdout, `"name":"status"`) {
					t.Errorf("db.table describe: %s", stdout)
				}
				stdout, _, ok = e.run("schema", "databases", "-c", "w", "--pattern", "CP_*")
				if !ok || len(records(t, stdout)) != 3 {
					t.Errorf("schema databases --pattern: %s", stdout)
				}
				stdout, _, _ = e.run("schema", "databases", "-c", "w")
				if strings.Contains(stdout, `"name":"mysql"`) {
					t.Errorf("system databases listed without --include-system: %s", stdout)
				}
				stdout, _, _ = e.run("run", "-c", "w", "-D", "cp_src", "SELECT COUNT(*) AS n FROM orders")
				if !strings.Contains(stdout, `"n":3`) {
					t.Errorf("run -D: %s", stdout)
				}
			})

			t.Run("restricted user sees only granted databases", func(t *testing.T) {
				e.exec("DROP USER IF EXISTS 'cp_limited'@'%'")
				e.exec("CREATE USER 'cp_limited'@'%' IDENTIFIED BY 'limited'")
				e.exec("GRANT SELECT ON cp_src.* TO 'cp_limited'@'%'")
				stdout, _, ok := e.run("schema", "databases", "-c", "restricted")
				recs := records(t, stdout)
				if !ok || len(recs) != 1 || recs[0]["name"] != "cp_src" {
					t.Errorf("restricted databases: %s", stdout)
				}
				stdout, _, ok = e.run("schema", "tables", "-c", "restricted", "-D", "cp_src")
				if !ok || len(records(t, stdout)) != 5 {
					t.Errorf("restricted -D: %s", stdout)
				}
			})

			t.Run("schema diff", func(t *testing.T) {
				stdout, stderr, ok := e.run(diffArgs()...)
				if !ok {
					t.Fatalf("diff failed: %s", stderr)
				}
				receipt := last(t, stdout)
				if receipt["only_in_source"].(float64) < 4 || receipt["destructive"].(float64) != 2 {
					t.Errorf("receipt = %v", receipt)
				}
				var narrowed bool
				for _, r := range records(t, stdout) {
					if r["object"] == "column" && r["name"] == "name" && r["destructive"] == true {
						narrowed = true
					}
				}
				if !narrowed {
					t.Errorf("varchar(64)->varchar(32) not flagged destructive:\n%s", stdout)
				}
				sqlOut, _, ok := e.run(diffArgs("--format", "sql")...)
				if !ok || !strings.Contains(sqlOut, "-- destructive") || !strings.Contains(sqlOut, "CREATE TABLE `cp_dst`.`orders`") {
					t.Errorf("--format sql:\n%s", sqlOut)
				}
			})

			t.Run("gates", func(t *testing.T) {
				cases := []struct {
					name    string
					args    []string
					fixable string
					contain string
				}{
					{"modes required", copyArgs(), "agent", "--structure is required"},
					{"read-only credential", []string{"copy", "--from", "w", "--to", "ro", "--schema", "cp_src", "--to-schema", "cp_dst", "--structure", "sync", "--data", "none"}, "human", "writePermission"},
					{"ad-hoc target", []string{"copy", "--from", "w", "--to", "mysql://root:test@127.0.0.1:" + s.port, "--schema", "cp_src", "--to-schema", "cp_dst", "--structure", "sync", "--data", "none"}, "human", "ad-hoc"},
					{"same schema", []string{"copy", "--from", "w", "--to", "w", "--schema", "cp_src", "--structure", "sync", "--data", "none"}, "agent", "same schema"},
					{"no key", copyArgs("--structure", "sync", "--data", "upsert"), "agent", "nokey"},
					{"destructive", copyArgs("--structure", "sync", "--data", "none"), "agent", "lose data"},
					{"missing from", []string{"copy", "--to", "w", "--schema", "cp_src", "--structure", "sync", "--data", "none"}, "agent", "--from"},
				}
				for _, c := range cases {
					_, stderr, ok := e.run(c.args...)
					if ok {
						t.Errorf("%s: succeeded", c.name)
						continue
					}
					err := errorOf(t, stderr)
					if err["fixable_by"] != c.fixable || !strings.Contains(err["error"].(string), c.contain) {
						t.Errorf("%s: %v", c.name, err)
					}
				}
			})

			t.Run("dry run writes nothing", func(t *testing.T) {
				before := e.dump("cp_dst", "customer")
				stdout, stderr, ok := e.run(copyArgs("--structure", "sync", "--data", "sync", "--exclude-tables", "nokey", "--dry-run")...)
				if !ok {
					t.Fatalf("dry run: %s", stderr)
				}
				receipt := last(t, stdout)
				if receipt["dry_run"] != true || receipt["requires_allow_destructive"] != true {
					t.Errorf("receipt = %v", receipt)
				}
				if e.dump("cp_dst", "customer") != before {
					t.Error("dry run changed the target")
				}
				// A dry run only reads the target, so a read-only
				// credential previews it, the way schema diff would.
				_, stderr, ok = e.run("copy", "--from", "w", "--to", "ro", "--schema", "cp_src", "--to-schema", "cp_dst",
					"--structure", "sync", "--data", "sync", "--exclude-tables", "nokey", "--dry-run")
				if !ok {
					t.Errorf("dry run into a read-only target: %s", stderr)
				}
			})

			t.Run("structure sync, data sync", func(t *testing.T) {
				stdout, stderr, ok := e.run(copyArgs("--structure", "sync", "--data", "sync", "--exclude-tables", "nokey", "--allow-destructive")...)
				if !ok {
					t.Fatalf("copy: %s\n%s", stderr, stdout)
				}
				e.assertSameRows("customer", "orders", "strkey", "empty_t")
				stdout, _, _ = e.run(diffArgs("--exclude-tables", "nokey")...)
				if r := last(t, stdout); r["different"].(float64) != 0 || r["only_in_source"].(float64) != 0 {
					t.Errorf("rediff after sync: %s", stdout)
				}
				// Running again changes nothing.
				stdout, _, _ = e.run(copyArgs("--structure", "sync", "--data", "sync", "--exclude-tables", "nokey", "--allow-destructive")...)
				r := last(t, stdout)
				if r["inserted"].(float64)+r["updated"].(float64)+r["deleted"].(float64) != 0 || r["statements"].(float64) != 0 {
					t.Errorf("second sync not a no-op: %v", r)
				}
				var ai []uint64
				rows, _ := e.db.Query("SELECT AUTO_INCREMENT FROM information_schema.tables WHERE TABLE_NAME='orders' AND TABLE_SCHEMA IN ('cp_src','cp_dst')")
				for rows.Next() {
					var v uint64
					_ = rows.Scan(&v)
					ai = append(ai, v)
				}
				_ = rows.Close()
				if len(ai) != 2 || ai[0] != ai[1] {
					t.Errorf("AUTO_INCREMENT not realigned: %v", ai)
				}
			})

			t.Run("sync keeps keys that differ only by collation", func(t *testing.T) {
				// Binary order puts source 'B' before target 'b', which the
				// merge sees as target-only. With one-row batches the upsert
				// of 'B' lands on 'b' (case-insensitive) before 'b' is
				// queued for delete, so a one-pass merge loses the row.
				e.exec("DELETE FROM cp_dst.strkey; INSERT INTO cp_dst.strkey VALUES ('A',9),('b',2),('z',5)")
				_, stderr, ok := e.run(copyArgs("--structure", "none", "--data", "sync", "--tables", "strkey", "--batch-rows", "1", "--allow-destructive")...)
				if !ok {
					t.Fatalf("copy: %s", stderr)
				}
				e.assertSameRows("strkey")
			})

			t.Run("sync --no-delete keeps target rows", func(t *testing.T) {
				e.exec("INSERT INTO cp_dst.strkey VALUES ('dev', 1)")
				_, stderr, ok := e.run(copyArgs("--structure", "none", "--data", "sync", "--no-delete", "--tables", "strkey")...)
				if !ok {
					t.Fatalf("copy: %s", stderr)
				}
				if !strings.Contains(e.dump("cp_dst", "strkey"), `"dev"`) {
					t.Error("--no-delete deleted a target-only row")
				}
			})

			t.Run("secondary unique index never redirects a write", func(t *testing.T) {
				// Target-only row 900 holds the email source row 2 needs.
				// INSERT ... ON DUPLICATE KEY UPDATE would land row 2 on row
				// 900 (any unique index matches), and sync would then delete
				// 900 as target-only: row 2 lost, counted as inserted.
				e.exec(`CREATE TABLE cp_src.uq (id INT PRIMARY KEY, email VARCHAR(16) NOT NULL, v INT, UNIQUE KEY uq_email (email));
					CREATE TABLE cp_dst.uq LIKE cp_src.uq;
					INSERT INTO cp_src.uq VALUES (1,'e1',1),(2,'a',2),(3,'e3',3),(4,'e4',4),(5,'e5',5)`)
				defer e.exec("DROP TABLE cp_src.uq; DROP TABLE cp_dst.uq")
				reset := func() { e.exec("DELETE FROM cp_dst.uq; INSERT INTO cp_dst.uq VALUES (900,'a',99)") }

				reset()
				_, stderr, ok := e.run(copyArgs("--structure", "none", "--data", "sync", "--tables", "uq", "--batch-rows", "1", "--allow-destructive")...)
				if !ok {
					t.Fatalf("sync: %s", stderr)
				}
				e.assertSameRows("uq") // 900 deleted first, then row 2 inserted

				// upsert never deletes, so row 2 collides with 900 on the
				// email: an error and a rolled-back table, not a silent
				// overwrite.
				reset()
				if _, _, ok := e.run(copyArgs("--structure", "none", "--data", "upsert", "--tables", "uq", "--batch-rows", "1")...); ok {
					t.Error("upsert colliding on a secondary unique index should fail")
				}
				if got := e.dump("cp_dst", "uq"); got != `"900"|"a"|"99"|`+"\n" {
					t.Errorf("target after a failed upsert:\n%s", got)
				}
			})

			t.Run("append duplicates with --continue-on-error", func(t *testing.T) {
				stdout, _, ok := e.run(copyArgs("--structure", "none", "--data", "append", "--tables", "customer,empty_t", "--continue-on-error")...)
				if ok {
					t.Error("append over existing keys should exit non-zero")
				}
				r := last(t, stdout)
				failures, _ := r["failures"].([]any)
				if len(failures) != 1 || r["tables"].(float64) != 2 {
					t.Errorf("receipt = %v", r)
				}
			})

			t.Run("upsert with --where", func(t *testing.T) {
				e.exec("DELETE FROM cp_dst.orders")
				stdout, stderr, ok := e.run(copyArgs("--structure", "none", "--data", "upsert", "--tables", "orders", "--where", "orders=amount > 100")...)
				if !ok {
					t.Fatalf("upsert: %s", stderr)
				}
				if r := last(t, stdout); r["upserted"].(float64) != 2 {
					t.Errorf("receipt = %v", r)
				}
			})

			t.Run("--where filters the source, not the rows it updates", func(t *testing.T) {
				// Target row 1 no longer matches amount > 100, but it is the
				// row source row 1 (which does) must update, not a new one.
				for _, mode := range []string{"upsert", "sync"} {
					e.exec("UPDATE cp_dst.orders SET amount = 1 WHERE id = 1")
					args := copyArgs("--structure", "none", "--data", mode, "--tables", "orders", "--where", "orders=amount > 100", "--allow-destructive")
					stdout, stderr, ok := e.run(append(args, "--dry-run")...)
					if !ok {
						t.Fatalf("%s dry run: %s", mode, stderr)
					}
					planned := last(t, stdout)
					stdout, stderr, ok = e.run(args...)
					if !ok {
						t.Fatalf("%s: %s", mode, stderr)
					}
					r := last(t, stdout)
					if strings.Contains(e.dump("cp_dst", "orders"), `"1"|"1"|"1.00"`) {
						t.Errorf("%s left target row 1 stale:\n%s", mode, e.dump("cp_dst", "orders"))
					}
					for _, k := range []string{"inserted", "updated", "deleted", "upserted"} {
						if planned[k] != r[k] {
							t.Errorf("%s %s: dry run planned %v, run did %v", mode, k, planned[k], r[k])
						}
					}
				}
			})

			t.Run("--where resolves unqualified names in each schema", func(t *testing.T) {
				where := "orders=customer_id IN (SELECT id FROM customer WHERE name = 'Alice')"
				stdout, stderr, ok := e.run(copyArgs("--structure", "none", "--data", "upsert", "--tables", "orders", "--where", where)...)
				if !ok {
					t.Fatalf("upsert: %s", stderr)
				}
				// The previous run upserted these rows already: unchanged
				// rows are compared, not rewritten.
				if r := last(t, stdout); r["upserted"] != nil || r["rows_read"].(float64) != 2 {
					t.Errorf("receipt = %v", r)
				}
				// sync also reads the target with the condition.
				if _, stderr, ok := e.run(copyArgs("--structure", "none", "--data", "sync", "--tables", "orders", "--where", where, "--dry-run")...); !ok {
					t.Errorf("sync dry run: %s", stderr)
				}
			})

			t.Run("backslashes survive a NO_BACKSLASH_ESCAPES source", func(t *testing.T) {
				var mode string
				if err := e.db.QueryRow("SELECT @@GLOBAL.sql_mode").Scan(&mode); err != nil {
					t.Fatal(err)
				}
				// Created before the mode change, so the literal is read with
				// backslash escapes whichever pooled connection runs it.
				e.exec(`DROP TABLE IF EXISTS cp_src.bs, cp_dst.bs; CREATE TABLE cp_src.bs (id INT PRIMARY KEY, p VARCHAR(20) DEFAULT 'C:\\tmp' COMMENT 'a\\b')`)
				t.Cleanup(func() { _, _ = e.db.Exec("DROP TABLE IF EXISTS cp_src.bs, cp_dst.bs") })
				// The CLI opens new sessions, which take the global mode.
				e.exec("SET GLOBAL sql_mode = CONCAT(@@GLOBAL.sql_mode, ',NO_BACKSLASH_ESCAPES')")
				t.Cleanup(func() {
					_, _ = e.db.Exec("SET GLOBAL sql_mode = ?", mode)
					// Drop pooled sessions that may have taken the changed mode.
					e.db.SetMaxIdleConns(0)
					e.db.SetMaxIdleConns(2)
				})
				if _, stderr, ok := e.run(copyArgs("--structure", "sync", "--data", "none", "--tables", "bs")...); !ok {
					t.Fatalf("copy: %s", stderr)
				}
				var got []string
				rows, err := e.db.Query("SELECT CONCAT(TABLE_SCHEMA, '=', COLUMN_DEFAULT, '|', COLUMN_COMMENT) FROM information_schema.columns WHERE TABLE_NAME = 'bs' AND COLUMN_NAME = 'p' AND TABLE_SCHEMA IN ('cp_src', 'cp_dst') ORDER BY TABLE_SCHEMA DESC")
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var v string
					_ = rows.Scan(&v)
					got = append(got, v)
				}
				_ = rows.Close()
				// MariaDB reports COLUMN_DEFAULT as a quoted expression; compare
				// the sides, and the comment's single backslash.
				if len(got) != 2 || strings.TrimPrefix(got[0], "cp_src=") != strings.TrimPrefix(got[1], "cp_dst=") ||
					!strings.HasSuffix(got[1], `|a\b`) {
					t.Errorf("source and target column = %q", got)
				}
			})

			t.Run("structure none incompatibility", func(t *testing.T) {
				e.exec("ALTER TABLE cp_dst.customer DROP COLUMN status")
				_, stderr, ok := e.run(copyArgs("--structure", "none", "--data", "append", "--tables", "customer")...)
				if ok || !strings.Contains(errorOf(t, stderr)["error"].(string), "lacks columns status") {
					t.Errorf("stderr = %s", stderr)
				}
			})

			t.Run("recreate and replace", func(t *testing.T) {
				// The dry run counts the rows recreate discards; the real
				// run only marks the tables cleared.
				stdout, stderr, ok := e.run(copyArgs("--structure", "recreate", "--data", "replace", "--allow-destructive", "--dry-run")...)
				if !ok {
					t.Fatalf("dry run: %s", stderr)
				}
				if r := last(t, stdout); r["deleted"].(float64) == 0 || r["cleared"] == nil {
					t.Errorf("dry-run receipt = %v", r)
				}
				stdout, stderr, ok = e.run(copyArgs("--structure", "recreate", "--data", "replace", "--allow-destructive")...)
				if !ok {
					t.Fatalf("recreate: %s", stderr)
				}
				e.assertSameRows("customer", "orders", "strkey", "empty_t", "nokey")
				for _, r := range records(t, stdout) {
					if r["table"] == "customer" && (r["structure"] != "recreated" || r["cleared"] != true || r["deleted"].(float64) != 0) {
						t.Errorf("customer record = %v", r)
					}
					if r["table"] == "nokey" && (r["structure"] != "created" || r["cleared"] != nil) {
						t.Errorf("nokey record = %v", r)
					}
				}
				// replace over existing tables truncates them uncounted.
				stdout, stderr, ok = e.run(copyArgs("--structure", "none", "--data", "replace", "--tables", "customer,orders", "--allow-destructive")...)
				if !ok {
					t.Fatalf("replace: %s", stderr)
				}
				if r := last(t, stdout); r["cleared"].(float64) != 2 || r["deleted"].(float64) != 0 {
					t.Errorf("replace receipt = %v", r)
				}
				e.assertSameRows("customer", "orders")
			})

			t.Run("drop extra", func(t *testing.T) {
				_, stderr, ok := e.run(copyArgs("--structure", "sync", "--data", "none", "--drop-extra", "--allow-destructive")...)
				if !ok {
					t.Fatalf("drop extra: %s", stderr)
				}
				var n int
				_ = e.db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE TABLE_SCHEMA='cp_dst' AND TABLE_NAME='dst_only'").Scan(&n)
				if n != 0 {
					t.Error("dst_only survived --drop-extra")
				}
			})
		})
	}
}

// TestDockerCopyMariaDBToMySQL copies across server variants: MariaDB's
// default sql_mode carries NO_AUTO_CREATE_USER, which MySQL 8 refuses, so the
// writer session must not take the source's mode wholesale.
func TestDockerCopyMariaDBToMySQL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping docker integration tests in short mode")
	}
	if !isPortOpen("13306") || !isPortOpen("13307") {
		t.Skip("mysql and mariadb containers not both available")
	}
	bin := buildBinary(t)
	maria := newCopyEnv(t, bin, copyServer{"mariadb", "mariadb", "13307"})
	my := newCopyEnv(t, bin, copyServer{"mysql", "mysql", "13306"})
	// utf8mb4_general_ci: MariaDB 11's default utf8mb4_uca1400_ai_ci is
	// unknown to MySQL, a separate cross-variant gap.
	maria.exec(`DROP DATABASE IF EXISTS cp_x; CREATE DATABASE cp_x CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
		CREATE TABLE cp_x.t (id INT PRIMARY KEY, name VARCHAR(16), at DATETIME);
		SET SESSION sql_mode = '';
		INSERT INTO cp_x.t VALUES (1,'a','2026-01-02 03:04:05'),(2,NULL,'0000-00-00 00:00:00')`)
	my.exec("DROP DATABASE IF EXISTS cp_x; CREATE DATABASE cp_x")
	defer maria.exec("DROP DATABASE cp_x")
	defer my.exec("DROP DATABASE cp_x")

	var mode string
	if err := maria.db.QueryRow("SELECT @@GLOBAL.sql_mode").Scan(&mode); err != nil || !strings.Contains(mode, "NO_AUTO_CREATE_USER") {
		t.Skipf("MariaDB's sql_mode lacks NO_AUTO_CREATE_USER (%q, %v): the case does not arise", mode, err)
	}
	cfg := `{"connections":{
		"maria":{"driver":"mariadb","host":"127.0.0.1","port":13307,"credential":"rw"},
		"my":{"driver":"mysql","host":"127.0.0.1","port":13306,"credential":"rw"}
	},"settings":{}}`
	if err := os.WriteFile(filepath.Join(my.xdg, "agent-sql", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, ok := my.run("copy", "--from", "maria", "--to", "my", "--schema", "cp_x",
		"--structure", "recreate", "--data", "replace", "--allow-destructive")
	if !ok {
		t.Fatalf("copy: %s", stderr)
	}
	if a, b := maria.dump("cp_x", "t"), my.dump("cp_x", "t"); a != b {
		t.Errorf("rows differ:\nmariadb:\n%s\nmysql:\n%s", a, b)
	}
}

// TestDockerMySQLServerIdentity checks the same-server probes copy uses:
// MySQL's server_uuid (MariaDB falls back to hostname:port), and the user
// lock marker held by one session and seen from another route.
func TestDockerMySQLServerIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping docker integration tests in short mode")
	}
	if !isPortOpen("13306") || !isPortOpen("13307") {
		t.Skip("mysql and mariadb containers not both available")
	}
	ctx := context.Background()
	open := func(host string, port int, variant string) driver.Connection {
		t.Helper()
		c, err := mysqldrv.Connect(mysqldrv.Opts{Host: host, Port: port, Username: "root", Password: "test", Readonly: true, Variant: variant})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	my := open("127.0.0.1", 13306, "mysql")
	mySame := open("localhost", 13306, "mysql")
	maria := open("127.0.0.1", 13307, "mariadb")

	id, err := my.(driver.SchemaIntrospector).ServerIdentity(ctx)
	if err != nil || len(id) != 36 {
		t.Errorf("mysql identity = %q, %v; want its server_uuid", id, err)
	}
	if id, err := maria.(driver.SchemaIntrospector).ServerIdentity(ctx); err != nil || !strings.HasSuffix(id, ":3306") {
		t.Errorf("mariadb identity = %q, %v; want hostname:port", id, err)
	}

	release, err := my.(driver.SessionMarker).HoldMarker(ctx, 424242)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := mySame.(driver.SessionMarker).MarkerHeld(ctx, 424242); err != nil || !held {
		t.Errorf("same server: held=%v err=%v", held, err)
	}
	if held, err := maria.(driver.SessionMarker).MarkerHeld(ctx, 424242); err != nil || held {
		t.Errorf("other server: held=%v err=%v", held, err)
	}
	release()
	if held, err := mySame.(driver.SessionMarker).MarkerHeld(ctx, 424242); err != nil || held {
		t.Errorf("after release: held=%v err=%v", held, err)
	}
}

// TestDockerCopyReviewCases covers two plans the main fixture does not reach:
// --data replace must leave a table whose ALTER failed untouched, and a
// --dry-run sync into a table that lacks the key yet must count the deletes
// the real run makes.
func TestDockerCopyReviewCases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping docker integration tests in short mode")
	}
	bin := buildBinary(t)
	for _, s := range copyServers {
		t.Run(s.name, func(t *testing.T) {
			if !isPortOpen(s.port) {
				t.Skipf("%s container not available on port %s", s.name, s.port)
			}
			e := newCopyEnv(t, bin, s)
			e.exec(`DROP DATABASE IF EXISTS cp_r_src; DROP DATABASE IF EXISTS cp_r_dst;
				CREATE DATABASE cp_r_src; CREATE DATABASE cp_r_dst;
				CREATE TABLE cp_r_src.u (id INT PRIMARY KEY, email VARCHAR(16), UNIQUE KEY uq_email (email));
				CREATE TABLE cp_r_src.ok_t (id INT PRIMARY KEY);
				CREATE TABLE cp_r_src.k (id INT PRIMARY KEY, v INT);
				INSERT INTO cp_r_src.u VALUES (1,'a'),(2,'b');
				INSERT INTO cp_r_src.ok_t VALUES (1),(2);
				INSERT INTO cp_r_src.k VALUES (1,1),(2,2);
				CREATE TABLE cp_r_dst.u (id INT PRIMARY KEY, email VARCHAR(16));
				CREATE TABLE cp_r_dst.ok_t (id INT PRIMARY KEY);
				CREATE TABLE cp_r_dst.k (v INT);
				INSERT INTO cp_r_dst.u VALUES (7,'x'),(8,'x');
				INSERT INTO cp_r_dst.ok_t VALUES (9);
				INSERT INTO cp_r_dst.k VALUES (5),(6),(7);`)
			defer e.exec("DROP DATABASE cp_r_src; DROP DATABASE cp_r_dst")
			base := []string{"copy", "--from", "w", "--to", "w", "--schema", "cp_r_src", "--to-schema", "cp_r_dst"}
			args := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
			table := func(stdout, name string) map[string]any {
				for _, r := range records(t, stdout) {
					if r["table"] == name {
						return r
					}
				}
				t.Fatalf("no record for %s:\n%s", name, stdout)
				return nil
			}

			t.Run("dry-run sync counts deletes when the key is new", func(t *testing.T) {
				for _, c := range []struct {
					where             []string
					deleted, inserted float64
				}{
					{nil, 3, 2},
					{[]string{"--where", "k=v > 5"}, 2, 0},
					// id is added by the structure phase: an upper bound.
					{[]string{"--where", "k=id > 1"}, 3, 1},
				} {
					extra := append([]string{"--structure", "sync", "--data", "sync", "--tables", "k", "--dry-run"}, c.where...)
					stdout, stderr, ok := e.run(args(extra...)...)
					if !ok {
						t.Fatalf("dry run %v: %s", c.where, stderr)
					}
					if r := table(stdout, "k"); r["deleted"].(float64) != c.deleted || r["inserted"].(float64) != c.inserted {
						t.Errorf("dry run %v: %v", c.where, r)
					}
				}
			})

			t.Run("replace keeps a table whose ALTER failed", func(t *testing.T) {
				before := e.dump("cp_r_dst", "u")
				stdout, _, ok := e.run(args("--structure", "sync", "--data", "replace", "--tables", "u,ok_t",
					"--allow-destructive", "--continue-on-error")...)
				if ok {
					t.Error("a failed ALTER should exit non-zero")
				}
				if after := e.dump("cp_r_dst", "u"); after != before {
					t.Errorf("failed table was cleared:\nbefore:\n%s\nafter:\n%s\n%s", before, after, stdout)
				}
				if src, dst := e.dump("cp_r_src", "ok_t"), e.dump("cp_r_dst", "ok_t"); src != dst {
					t.Errorf("ok_t not copied:\n%s\n%s", src, dst)
				}
			})
		})
	}
}
