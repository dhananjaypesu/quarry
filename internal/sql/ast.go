package sql

import "github.com/dhananjaypesu/quarry/internal/types"

// Statement is any complete SQL statement.
type Statement interface{ stmt() }

// Expr is a scalar expression. The AST keeps expressions untyped: which column
// a name refers to and whether an operator is legal on it are questions for
// the binder, which has the catalog and can produce an error that names the
// table.
type Expr interface{ expr() }

// ---- expressions ----

// Literal is a constant.
type Literal struct{ Value types.Value }

// ColumnRef is a possibly qualified column name: t.c or c.
type ColumnRef struct {
	Table string
	Name  string
}

// BinaryExpr covers arithmetic, comparison and boolean connectives; the parser
// does not separate them because they differ only in precedence, which the
// precedence table already encodes.
type BinaryExpr struct {
	Op    string
	Left  Expr
	Right Expr
}

// UnaryExpr is -x or NOT x.
type UnaryExpr struct {
	Op string
	X  Expr
}

// IsNullExpr is x IS NULL / x IS NOT NULL, which is a distinct node because it
// is the one comparison that is *not* three-valued: it always answers.
type IsNullExpr struct {
	X      Expr
	Negate bool
}

// InExpr is x IN (a, b, c).
type InExpr struct {
	X      Expr
	List   []Expr
	Negate bool
}

// BetweenExpr is x BETWEEN lo AND hi, kept whole rather than desugared so the
// planner can recognize it as a range and turn it into a seek.
type BetweenExpr struct {
	X      Expr
	Lo, Hi Expr
	Negate bool
}

// FuncCall is an aggregate or scalar function call. Star records COUNT(*).
type FuncCall struct {
	Name string
	Args []Expr
	Star bool
}

func (*Literal) expr()     {}
func (*ColumnRef) expr()   {}
func (*BinaryExpr) expr()  {}
func (*UnaryExpr) expr()   {}
func (*IsNullExpr) expr()  {}
func (*InExpr) expr()      {}
func (*BetweenExpr) expr() {}
func (*FuncCall) expr()    {}

// ---- statements ----

// ColumnDef is one column in CREATE TABLE.
type ColumnDef struct {
	Name       string
	Type       types.Type
	TypeName   string
	NotNull    bool
	PrimaryKey bool
}

type CreateTable struct {
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	PrimaryKey  []string // set by a table-level PRIMARY KEY (a, b) clause
}

type DropTable struct {
	Name     string
	IfExists bool
}

type CreateIndex struct {
	Name        string
	Table       string
	Columns     []string
	Unique      bool
	IfNotExists bool
}

type Insert struct {
	Table   string
	Columns []string // empty means "every column, in declaration order"
	Rows    [][]Expr
}

type Assignment struct {
	Column string
	Value  Expr
}

type Update struct {
	Table string
	Set   []Assignment
	Where Expr
}

type Delete struct {
	Table string
	Where Expr
}

// SelectItem is one entry of the select list. Star and TableStar carry * and
// t.*, which expand only once the binder knows the table's columns.
type SelectItem struct {
	Expr      Expr
	Alias     string
	Star      bool
	TableStar string
}

// TableRef is a FROM entry: a base table plus, for joins, the right side.
type TableRef struct {
	Name  string
	Alias string
	// Joins are represented as a left-deep chain hanging off the first table,
	// which is the only shape the parser can build from "a JOIN b JOIN c" and
	// is exactly what the executor wants to consume.
	Joins []JoinClause
}

type JoinClause struct {
	Type  JoinType
	Right TableRef
	On    Expr
}

type JoinType uint8

const (
	InnerJoin JoinType = iota
	LeftJoin
	CrossJoin
)

type OrderItem struct {
	Expr Expr
	Desc bool
}

type Select struct {
	Distinct bool
	Items    []SelectItem
	From     *TableRef
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderItem
	Limit    Expr
	Offset   Expr
}

// Explain wraps another statement and asks for its plan instead of its rows.
type Explain struct{ Stmt Statement }

// Transaction control.
type Begin struct{}
type Commit struct{}
type Rollback struct{}

// ShowTables and Describe are the two introspection statements the shell needs
// to be usable; they read the catalog like any other query.
type ShowTables struct{}
type Describe struct{ Table string }

func (*CreateTable) stmt() {}
func (*DropTable) stmt()   {}
func (*CreateIndex) stmt() {}
func (*Insert) stmt()      {}
func (*Update) stmt()      {}
func (*Delete) stmt()      {}
func (*Select) stmt()      {}
func (*Explain) stmt()     {}
func (*Begin) stmt()       {}
func (*Commit) stmt()      {}
func (*Rollback) stmt()    {}
func (*ShowTables) stmt()  {}
func (*Describe) stmt()    {}
