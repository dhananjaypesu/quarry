package quarry_test

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/dhananjaypesu/quarry"
)

func open(t *testing.T) *quarry.DB {
	t.Helper()
	db, err := quarry.Open(t.TempDir(), quarry.DefaultOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mustExec runs a script and fails the test if any statement errors.
func mustExec(t *testing.T, db *quarry.DB, script string) {
	t.Helper()
	if _, err := db.Exec(script); err != nil {
		t.Fatalf("exec %q: %v", firstLine(script), err)
	}
}

// rows renders a query's result as one string per row, which makes an expected
// result a readable literal instead of a nested slice.
func rows(t *testing.T, db *quarry.DB, q string) []string {
	t.Helper()
	res, err := db.Query(q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		parts := make([]string, len(r))
		for i, v := range r {
			parts[i] = v.String()
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return out
}

func wantRows(t *testing.T, db *quarry.DB, q string, want ...string) {
	t.Helper()
	got := rows(t, db, q)
	if len(got) != len(want) {
		t.Fatalf("%q returned %d rows %v, want %d %v", q, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%q row %d = %q, want %q", q, i, got[i], want[i])
		}
	}
}

func wantError(t *testing.T, db *quarry.DB, stmt, contains string) {
	t.Helper()
	_, err := db.Exec(stmt)
	if err == nil {
		t.Fatalf("%q was accepted, expected an error mentioning %q", firstLine(stmt), contains)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("%q failed with %q, expected it to mention %q", firstLine(stmt), err, contains)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

const schema = `
CREATE TABLE users (id INT PRIMARY KEY, name TEXT NOT NULL, city TEXT, age INT);
INSERT INTO users VALUES (1,'ada','london',36),(2,'grace','nyc',45),(3,'alan','london',41),(4,'edsger',NULL,72);
`

func TestSelectProjectFilterOrderLimit(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)

	wantRows(t, db, "SELECT name FROM users ORDER BY age DESC LIMIT 2", "edsger", "grace")
	wantRows(t, db, "SELECT name FROM users ORDER BY age DESC LIMIT 2 OFFSET 1", "grace", "alan")
	wantRows(t, db, "SELECT name, age + 1 FROM users WHERE age < 42 ORDER BY name", "ada|37", "alan|42")
	wantRows(t, db, "SELECT DISTINCT city FROM users ORDER BY city", "NULL", "london", "nyc")
	wantRows(t, db, "SELECT name FROM users WHERE name LIKE 'a%' ORDER BY name", "ada", "alan")
	wantRows(t, db, "SELECT name FROM users WHERE id IN (2, 4) ORDER BY id", "grace", "edsger")
	wantRows(t, db, "SELECT COUNT(*) FROM users WHERE city = 'nowhere'", "0")
}

// SQL comparisons against NULL are UNKNOWN, and a WHERE clause admits only
// rows for which the predicate is known TRUE. The row with a NULL city must
// therefore be absent from both a match and its negation.
func TestNullSemantics(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)

	wantRows(t, db, "SELECT name FROM users WHERE city = 'london' ORDER BY name", "ada", "alan")
	wantRows(t, db, "SELECT name FROM users WHERE city != 'london' ORDER BY name", "grace")
	wantRows(t, db, "SELECT name FROM users WHERE city IS NULL", "edsger")
	wantRows(t, db, "SELECT name FROM users WHERE NOT (city = 'london') ORDER BY name", "grace")
	// COUNT(*) counts rows; COUNT(x) counts non-NULL values of x.
	wantRows(t, db, "SELECT COUNT(*), COUNT(city) FROM users", "4|3")
	// NULL sorts first, matching the encoding's ordering.
	wantRows(t, db, "SELECT name FROM users ORDER BY city, name",
		"edsger", "ada", "alan", "grace")
	// UNKNOWN is not FALSE: OR with a true branch still returns the row.
	wantRows(t, db, "SELECT name FROM users WHERE city = 'nope' OR id = 4", "edsger")
}

func TestConstraintsAreEnforced(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)

	wantError(t, db, "INSERT INTO users VALUES (1,'dup','x',1)", "primary key")
	wantError(t, db, "INSERT INTO users VALUES (9,NULL,'x',1)", "cannot be NULL")
	wantError(t, db, "UPDATE users SET name = NULL WHERE id = 1", "cannot be NULL")
	wantError(t, db, "SELECT * FROM nosuch", "no such table")
	wantError(t, db, "SELECT nosuch FROM users", "no column")
	wantError(t, db, "SELECT * FROM users WHERE name > 5", "cannot compare")
	wantError(t, db, "SELECT name FROM users GROUP BY city", "must appear in GROUP BY")
	// A failed statement must leave nothing behind.
	wantRows(t, db, "SELECT COUNT(*) FROM users", "4")
	wantRows(t, db, "SELECT name FROM users WHERE id = 1", "ada")
}

func TestUpdateAndDelete(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)

	mustExec(t, db, "UPDATE users SET age = age + 1 WHERE city = 'london'")
	wantRows(t, db, "SELECT name, age FROM users ORDER BY id", "ada|37", "grace|45", "alan|42", "edsger|72")

	// Moving a primary key relocates the row rather than duplicating it.
	mustExec(t, db, "UPDATE users SET id = 10 WHERE id = 1")
	wantRows(t, db, "SELECT id, name FROM users ORDER BY id", "2|grace", "3|alan", "4|edsger", "10|ada")
	wantRows(t, db, "SELECT COUNT(*) FROM users", "4")

	mustExec(t, db, "DELETE FROM users WHERE city IS NULL")
	wantRows(t, db, "SELECT COUNT(*) FROM users", "3")
	mustExec(t, db, "DELETE FROM users")
	wantRows(t, db, "SELECT COUNT(*) FROM users", "0")
}

func TestAggregatesAndGrouping(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)

	wantRows(t, db, "SELECT COUNT(*), MIN(age), MAX(age), SUM(age) FROM users", "4|36|72|194")
	wantRows(t, db, "SELECT AVG(age) FROM users WHERE city = 'london'", "38.5")
	wantRows(t, db, "SELECT city, COUNT(*) n FROM users GROUP BY city ORDER BY n DESC, city",
		"london|2", "NULL|1", "nyc|1")
	wantRows(t, db, "SELECT city FROM users GROUP BY city HAVING COUNT(*) > 1", "london")
	// An ungrouped aggregate over no rows still produces one row.
	wantRows(t, db, "SELECT COUNT(*), SUM(age), AVG(age) FROM users WHERE id > 100", "0|NULL|NULL")
	// A grouped query over no rows produces none.
	wantRows(t, db, "SELECT city, COUNT(*) FROM users WHERE id > 100 GROUP BY city")
	// Grouping by an expression, and repeating an aggregate, both work.
	// The repeated aggregate is computed once and referenced twice.
	wantRows(t, db, "SELECT age / 10 decade, COUNT(*), COUNT(*) FROM users GROUP BY age / 10 ORDER BY decade",
		"3|1|1", "4|2|2", "7|1|1")
}

func TestJoins(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)
	mustExec(t, db, `
CREATE TABLE orders (id INT PRIMARY KEY, user_id INT, total FLOAT);
INSERT INTO orders VALUES (10,1,99.5),(11,1,20.0),(12,2,7.25),(13,99,1.0);
`)

	wantRows(t, db, "SELECT u.name, o.total FROM users u JOIN orders o ON o.user_id = u.id ORDER BY o.id",
		"ada|99.5", "ada|20", "grace|7.25")
	wantRows(t, db, "SELECT u.name, o.id FROM users u LEFT JOIN orders o ON o.user_id = u.id ORDER BY u.id, o.id",
		"ada|10", "ada|11", "grace|12", "alan|NULL", "edsger|NULL")
	wantRows(t, db, "SELECT u.name, SUM(o.total) s FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.name ORDER BY s DESC",
		"ada|119.5", "grace|7.25")
	wantRows(t, db, "SELECT COUNT(*) FROM users CROSS JOIN orders", "16")
	// A predicate in the ON clause that is not an equality stays a residual.
	wantRows(t, db, "SELECT u.name, o.total FROM users u JOIN orders o ON o.user_id = u.id AND o.total > 50 ORDER BY o.id",
		"ada|99.5")
}

func TestIndexesStayConsistentWithRows(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)
	mustExec(t, db, "CREATE INDEX users_city ON users (city)")

	// The index was built over rows that already existed.
	wantRows(t, db, "SELECT name FROM users WHERE city = 'london' ORDER BY name", "ada", "alan")
	if plan := explain(t, db, "SELECT name FROM users WHERE city = 'london'"); !strings.Contains(plan, "index users_city") {
		t.Fatalf("planner ignored the index:\n%s", plan)
	}

	// Every mutation has to maintain the index, or these two queries diverge.
	mustExec(t, db, "UPDATE users SET city = 'paris' WHERE name = 'ada'")
	mustExec(t, db, "DELETE FROM users WHERE name = 'alan'")
	mustExec(t, db, "INSERT INTO users VALUES (5,'barbara','london',60)")
	wantRows(t, db, "SELECT name FROM users WHERE city = 'london'", "barbara")
	wantRows(t, db, "SELECT name FROM users WHERE city = 'paris'", "ada")

	// The same question answered without the index must agree.
	viaScan := rows(t, db, "SELECT name FROM users WHERE city || '' = 'london'")
	viaIndex := rows(t, db, "SELECT name FROM users WHERE city = 'london'")
	if strings.Join(viaScan, ",") != strings.Join(viaIndex, ",") {
		t.Fatalf("index scan %v disagrees with full scan %v", viaIndex, viaScan)
	}
}

func TestUniqueIndex(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)
	mustExec(t, db, "CREATE UNIQUE INDEX users_name ON users (name)")

	wantError(t, db, "INSERT INTO users VALUES (5,'ada','x',1)", "unique")
	wantError(t, db, "UPDATE users SET name = 'ada' WHERE id = 2", "unique")
	mustExec(t, db, "INSERT INTO users VALUES (5,'barbara','x',1)")

	// NULLs are not equal to each other, so a unique index admits many of them.
	mustExec(t, db, "DELETE FROM users WHERE name = 'alan'") // london is duplicated
	mustExec(t, db, "CREATE UNIQUE INDEX users_city ON users (city)")
	mustExec(t, db, "INSERT INTO users VALUES (6,'niklaus',NULL,1)")
	mustExec(t, db, "INSERT INTO users VALUES (7,'donald',NULL,1)")
	wantRows(t, db, "SELECT COUNT(*) FROM users WHERE city IS NULL", "3")

	// A unique index cannot be created over rows that already collide: the
	// three rows added above all have age 1.
	wantError(t, db, "CREATE UNIQUE INDEX users_age ON users (age)", "unique")
	// The failed CREATE INDEX must not have left the index behind.
	wantError(t, db, "SELECT * FROM users WHERE age = 1 AND nosuch = 1", "no column")
}

func TestTransactions(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO users VALUES (5,'barbara','x',60)")
	mustExec(t, db, "UPDATE users SET age = 0 WHERE id = 1")
	// Read-your-own-writes: the statement sees the uncommitted rows.
	wantRows(t, db, "SELECT COUNT(*) FROM users", "5")
	wantRows(t, db, "SELECT age FROM users WHERE id = 1", "0")
	mustExec(t, db, "ROLLBACK")

	wantRows(t, db, "SELECT COUNT(*) FROM users", "4")
	wantRows(t, db, "SELECT age FROM users WHERE id = 1", "36")

	mustExec(t, db, "BEGIN")
	mustExec(t, db, "INSERT INTO users VALUES (5,'barbara','x',60)")
	mustExec(t, db, "COMMIT")
	wantRows(t, db, "SELECT COUNT(*) FROM users", "5")

	// DDL is transactional too: a rolled-back CREATE TABLE leaves no table.
	mustExec(t, db, "BEGIN")
	mustExec(t, db, "CREATE TABLE tmp (a INT PRIMARY KEY)")
	mustExec(t, db, "INSERT INTO tmp VALUES (1)")
	mustExec(t, db, "ROLLBACK")
	wantError(t, db, "SELECT * FROM tmp", "no such table")
}

func TestRowIDTablesAndReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := quarry.Open(dir, quarry.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	// A table without a primary key gets a hidden one, which must not appear
	// in SELECT *.
	mustExec(t, db, "CREATE TABLE log (msg TEXT, level INT)")
	mustExec(t, db, "INSERT INTO log VALUES ('a',1),('b',2),('c',1)")
	wantRows(t, db, "SELECT * FROM log", "a|1", "b|2", "c|1")
	wantRows(t, db, "SELECT COUNT(*) FROM log WHERE level = 1", "2")
	mustExec(t, db, "CREATE INDEX log_level ON log (level)")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Everything — schema, rows, indexes, the row id counter — must survive a
	// restart, because all of it lives in the store rather than in memory.
	db2, err := quarry.Open(dir, quarry.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	wantRows(t, db2, "SELECT * FROM log", "a|1", "b|2", "c|1")
	mustExec(t, db2, "INSERT INTO log VALUES ('d',2)")
	wantRows(t, db2, "SELECT COUNT(*) FROM log", "4")
	wantRows(t, db2, "SELECT msg FROM log WHERE level = 2 ORDER BY msg", "b", "d")
}

// A composite primary key is one encoded tuple, so a predicate that pins only
// its leading columns still narrows the scan to a prefix range.
func TestCompositePrimaryKey(t *testing.T) {
	db := open(t)
	mustExec(t, db, `
CREATE TABLE events (a INT, b TEXT, payload TEXT, PRIMARY KEY (a, b));
INSERT INTO events VALUES (1,'x','p1'),(1,'y','p2'),(2,'x','p3'),(10,'a','p4');
`)
	// Rows come back in key order, and an INT key sorts numerically rather
	// than lexicographically — 10 after 2, which a naive encoding would get
	// backwards.
	wantRows(t, db, "SELECT a, b FROM events", "1|x", "1|y", "2|x", "10|a")
	wantRows(t, db, "SELECT payload FROM events WHERE a = 1 AND b = 'y'", "p2")
	wantRows(t, db, "SELECT payload FROM events WHERE a = 1", "p1", "p2")

	if plan := explain(t, db, "SELECT * FROM events WHERE a = 1 AND b = 'y'"); !strings.Contains(plan, "lookup") {
		t.Fatalf("a fully pinned composite key did not become a lookup:\n%s", plan)
	}
	if plan := explain(t, db, "SELECT * FROM events WHERE a = 1"); !strings.Contains(plan, "primary key range") {
		t.Fatalf("a leading key column did not become a prefix range:\n%s", plan)
	}
	// The second key column alone cannot narrow anything.
	if plan := explain(t, db, "SELECT * FROM events WHERE b = 'x'"); !strings.Contains(plan, "full scan") {
		t.Fatalf("a trailing key column should not narrow the scan:\n%s", plan)
	}
	wantError(t, db, "INSERT INTO events VALUES (1,'x','dup')", "primary key")
}

func TestDropTableReclaimsTheKeyspace(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)
	mustExec(t, db, "CREATE INDEX users_city ON users (city)")
	mustExec(t, db, "DROP TABLE users")
	wantError(t, db, "SELECT * FROM users", "no such table")

	// A new table must not inherit the old one's rows through a reused id.
	mustExec(t, db, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)")
	wantRows(t, db, "SELECT COUNT(*) FROM users", "0")
}

// The planner must narrow a scan to a key range whenever the predicate allows
// it, and must fall back to a full scan when it does not.
func TestAccessPathSelection(t *testing.T) {
	db := open(t)
	mustExec(t, db, schema)
	mustExec(t, db, "CREATE INDEX users_city ON users (city)")

	cases := []struct{ query, want string }{
		{"SELECT * FROM users WHERE id = 3", "primary key lookup"},
		{"SELECT * FROM users WHERE id > 2 AND id <= 4", "primary key range"},
		{"SELECT * FROM users WHERE id BETWEEN 2 AND 3", "primary key range"},
		{"SELECT * FROM users WHERE city = 'london'", "index users_city"},
		{"SELECT * FROM users WHERE name = 'ada'", "full scan"},
		{"SELECT * FROM users WHERE id = 1 OR id = 3", "full scan"},
		{"SELECT * FROM users", "full scan"},
	}
	for _, c := range cases {
		if plan := explain(t, db, c.query); !strings.Contains(plan, c.want) {
			t.Errorf("%q planned as:\n%s\nwant %q", c.query, plan, c.want)
		}
	}

	// Narrowing the scan must not change the answer.
	wantRows(t, db, "SELECT id FROM users WHERE id > 2 AND id <= 4 ORDER BY id", "3", "4")
	wantRows(t, db, "SELECT id FROM users WHERE id BETWEEN 2 AND 3 ORDER BY id", "2", "3")
}

func explain(t *testing.T, db *quarry.DB, q string) string {
	t.Helper()
	res, err := db.Query("EXPLAIN " + q)
	if err != nil {
		t.Fatalf("explain %q: %v", q, err)
	}
	return res.Text
}

// TestRandomizedAgainstModel runs a random workload through the engine and
// through a plain Go map, then checks that both agree.
//
// This is the test that catches what hand-written cases do not: an index entry
// left behind by a deleted row, a range bound off by one key, an update that
// writes to the old key. The model is deliberately dumb — a map and a sort —
// so that when the two disagree, the engine is what is wrong.
func TestRandomizedAgainstModel(t *testing.T) {
	db := open(t)
	mustExec(t, db, `
CREATE TABLE kv (k INT PRIMARY KEY, tag TEXT, n INT);
CREATE INDEX kv_tag ON kv (tag);
`)
	model := map[int64]record{}
	tags := []string{"a", "b", "c"}
	rng := rand.New(rand.NewSource(20240822))

	for step := 0; step < 3000; step++ {
		k := rng.Int63n(60)
		switch rng.Intn(10) {
		case 0, 1, 2, 3:
			rec := record{tag: tags[rng.Intn(len(tags))], n: rng.Int63n(100)}
			if _, exists := model[k]; exists {
				mustExec(t, db, fmt.Sprintf("UPDATE kv SET tag = '%s', n = %d WHERE k = %d", rec.tag, rec.n, k))
			} else {
				mustExec(t, db, fmt.Sprintf("INSERT INTO kv VALUES (%d, '%s', %d)", k, rec.tag, rec.n))
			}
			model[k] = rec
		case 4, 5:
			mustExec(t, db, fmt.Sprintf("DELETE FROM kv WHERE k = %d", k))
			delete(model, k)
		case 6:
			// A range delete exercises the scan-then-write path with many rows.
			hi := k + rng.Int63n(10)
			mustExec(t, db, fmt.Sprintf("DELETE FROM kv WHERE k >= %d AND k <= %d", k, hi))
			for mk := k; mk <= hi; mk++ {
				delete(model, mk)
			}
		case 7:
			mustExec(t, db, fmt.Sprintf("UPDATE kv SET n = n + 1 WHERE k >= %d", k))
			for mk, rec := range model {
				if mk >= k {
					rec.n++
					model[mk] = rec
				}
			}
		case 8:
			checkFullScan(t, db, model)
		case 9:
			tag := tags[rng.Intn(len(tags))]
			checkIndexScan(t, db, model, tag)
			lo, hi := k, k+rng.Int63n(20)
			checkRange(t, db, model, lo, hi)
		}
	}
	checkFullScan(t, db, model)
	for _, tag := range tags {
		checkIndexScan(t, db, model, tag)
	}
}

// record is the model's view of one row.
type record struct {
	tag string
	n   int64
}

func checkFullScan(t *testing.T, db *quarry.DB, model map[int64]record) {
	t.Helper()
	var want []string
	for k, rec := range model {
		want = append(want, fmt.Sprintf("%d|%s|%d", k, rec.tag, rec.n))
	}
	sortByKey(want)
	got := rows(t, db, "SELECT k, tag, n FROM kv ORDER BY k")
	compare(t, "full scan", got, want)
}

func checkIndexScan(t *testing.T, db *quarry.DB, model map[int64]record, tag string) {
	t.Helper()
	var want []string
	for k, rec := range model {
		if rec.tag == tag {
			want = append(want, fmt.Sprintf("%d|%s|%d", k, rec.tag, rec.n))
		}
	}
	sortByKey(want)
	got := rows(t, db, fmt.Sprintf("SELECT k, tag, n FROM kv WHERE tag = '%s' ORDER BY k", tag))
	compare(t, "index scan on tag="+tag, got, want)
}

func checkRange(t *testing.T, db *quarry.DB, model map[int64]record, lo, hi int64) {
	t.Helper()
	var want []string
	for k, rec := range model {
		if k >= lo && k <= hi {
			want = append(want, fmt.Sprintf("%d|%s|%d", k, rec.tag, rec.n))
		}
	}
	sortByKey(want)
	got := rows(t, db, fmt.Sprintf("SELECT k, tag, n FROM kv WHERE k >= %d AND k <= %d ORDER BY k", lo, hi))
	compare(t, fmt.Sprintf("range [%d, %d]", lo, hi), got, want)
}

func sortByKey(rows []string) {
	sort.Slice(rows, func(i, j int) bool {
		var a, b int64
		fmt.Sscanf(rows[i], "%d|", &a)
		fmt.Sscanf(rows[j], "%d|", &b)
		return a < b
	})
}

func compare(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: engine returned %d rows, model has %d\nengine: %v\nmodel:  %v",
			what, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d: engine %q, model %q", what, i, got[i], want[i])
		}
	}
}
