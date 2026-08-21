package sql

import (
	"strings"
	"testing"
)

func TestParseAcceptsValidStatements(t *testing.T) {
	stmts := []string{
		"SELECT * FROM t",
		"select a, b as c from t where a = 1 and b like 'x%' order by a desc limit 10 offset 2",
		"SELECT t.*, u.name FROM t JOIN u ON t.id = u.id",
		"SELECT city, COUNT(*), AVG(age) FROM users GROUP BY city HAVING COUNT(*) > 1",
		"SELECT DISTINCT a FROM t WHERE a IN (1, 2, 3) AND b IS NOT NULL",
		"SELECT a FROM t WHERE a BETWEEN 1 AND 10 OR NOT (b < 2)",
		"INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')",
		"UPDATE t SET a = a + 1, b = 'z' WHERE a > 0",
		"DELETE FROM t WHERE a = 1",
		"CREATE TABLE t (a INT PRIMARY KEY, b TEXT NOT NULL, c FLOAT, d BOOL)",
		"CREATE TABLE IF NOT EXISTS t (a INT, b VARCHAR(255), PRIMARY KEY (a, b))",
		"CREATE UNIQUE INDEX i ON t (a, b)",
		"DROP TABLE IF EXISTS t",
		"EXPLAIN SELECT * FROM t WHERE a = 1",
		"BEGIN", "COMMIT", "ROLLBACK", "SHOW TABLES", "DESCRIBE t",
		"SELECT a FROM t -- trailing comment",
		"SELECT 'it''s' FROM t",
	}
	for _, s := range stmts {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q): %v", s, err)
		}
	}
}

func TestParseRejectsMalformedStatements(t *testing.T) {
	stmts := []string{
		"SELECT",
		"SELECT * FROM",
		"SELECT * FROM t WHERE",
		"INSERT INTO t VALUES",
		"CREATE TABLE t ()",
		"CREATE TABLE t (a NOTATYPE)",
		"SELECT * FROM t JOIN u",  // a join needs an ON clause
		"SELECT * FROM t ORDER a", // BY is not optional
		"SELECT 'unterminated FROM t",
		"SELECT * FROM t; SELECT",
		"DELETE t WHERE a = 1",
	}
	for _, s := range stmts {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted an invalid statement", s)
		}
	}
}

// Precedence is asserted through the tree's shape: AND must bind tighter than
// OR, comparison tighter than AND, and arithmetic tighter than comparison.
func TestOperatorPrecedence(t *testing.T) {
	st, err := Parse("SELECT a FROM t WHERE a = 1 + 2 * 3 OR b = 4 AND c = 5")
	if err != nil {
		t.Fatal(err)
	}
	where := st.(*Select).Where
	top, ok := where.(*BinaryExpr)
	if !ok || top.Op != "OR" {
		t.Fatalf("top of the tree is %#v, want OR", where)
	}
	if right, ok := top.Right.(*BinaryExpr); !ok || right.Op != "AND" {
		t.Fatalf("right branch is %#v, want AND", top.Right)
	}
	left := top.Left.(*BinaryExpr)
	if left.Op != "=" {
		t.Fatalf("left branch is %s, want =", left.Op)
	}
	add := left.Right.(*BinaryExpr)
	if add.Op != "+" {
		t.Fatalf("expected 1 + (2 * 3), found %s at the top", add.Op)
	}
	if mul, ok := add.Right.(*BinaryExpr); !ok || mul.Op != "*" {
		t.Fatalf("multiplication did not bind tighter than addition: %#v", add.Right)
	}
}

func TestParseManySplitsOnTokensNotText(t *testing.T) {
	// The semicolon inside the string literal must not split the script.
	stmts, err := ParseMany("INSERT INTO t VALUES ('a;b'); SELECT * FROM t;")
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2", len(stmts))
	}
	ins := stmts[0].(*Insert)
	if got := ins.Rows[0][0].(*Literal).Value.S; got != "a;b" {
		t.Fatalf("string literal became %q", got)
	}
}

func TestErrorsPointAtThePosition(t *testing.T) {
	_, err := Parse("SELECT * FROM t WHERE a = ")
	if err == nil || !strings.Contains(err.Error(), "position") {
		t.Fatalf("error %v does not name a position", err)
	}
}

func TestQuotedIdentifiersEscapeKeywords(t *testing.T) {
	st, err := Parse(`SELECT "select" FROM "from"`)
	if err != nil {
		t.Fatal(err)
	}
	sel := st.(*Select)
	if got := sel.Items[0].Expr.(*ColumnRef).Name; got != "select" {
		t.Fatalf("column is %q", got)
	}
	if sel.From.Name != "from" {
		t.Fatalf("table is %q", sel.From.Name)
	}
}
