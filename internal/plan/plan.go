// Package plan turns a parsed statement into a tree of executable nodes.
//
// There is no separate logical and physical plan here. A cost-based optimizer
// with statistics would need one — it rewrites a logical tree many times
// before choosing physical operators. This engine makes exactly two structural
// decisions (which access path a scan uses, and whether a join can hash) and
// both are made while the tree is built, so a single node type per operation
// keeps the code honest about what it actually does.
package plan

import (
	"fmt"
	"strings"

	"github.com/dhananjaypesu/quarry/internal/catalog"
	"github.com/dhananjaypesu/quarry/internal/expr"
	"github.com/dhananjaypesu/quarry/internal/sql"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// Col describes one column of a node's output.
type Col struct {
	Table string
	Name  string
	Type  types.Type
}

// Node is a plan node.
type Node interface {
	// Columns is the shape of the tuples this node produces.
	Columns() []Col
	// explain returns this node's own line and its children, which is what
	// EXPLAIN prints.
	explain() (string, []Node)
}

// Scan reads a table through one access path.
//
// Start and End are the byte range of the underlying store to walk. A full
// scan is the whole table prefix; a point lookup is a one-key range; an index
// scan walks the index prefix and follows each entry back to the row. The
// planner's only real decision is which of those it can prove is enough.
type Scan struct {
	Table *catalog.Table
	Alias string
	Index *catalog.Index // nil means the primary key
	Start []byte
	End   []byte
	// Filter is the part of the predicate the key range does not already
	// enforce, applied to each row as it is produced.
	Filter expr.Expr
	// Access is a human-readable summary for EXPLAIN.
	Access string
	Out    []Col
}

func (s *Scan) Columns() []Col { return s.Out }
func (s *Scan) explain() (string, []Node) {
	name := s.Table.Name
	if s.Alias != "" && !strings.EqualFold(s.Alias, s.Table.Name) {
		name += " AS " + s.Alias
	}
	line := fmt.Sprintf("Scan %s [%s]", name, s.Access)
	if s.Filter != nil {
		line += " filter=" + s.Filter.String()
	}
	return line, nil
}

// Filter drops rows that do not satisfy a predicate.
type Filter struct {
	Input Node
	Pred  expr.Expr
}

func (f *Filter) Columns() []Col { return f.Input.Columns() }
func (f *Filter) explain() (string, []Node) {
	return "Filter " + f.Pred.String(), []Node{f.Input}
}

// Project evaluates the select list.
type Project struct {
	Input Node
	Exprs []expr.Expr
	Out   []Col
}

func (p *Project) Columns() []Col { return p.Out }
func (p *Project) explain() (string, []Node) {
	parts := make([]string, len(p.Exprs))
	for i, e := range p.Exprs {
		parts[i] = e.String()
	}
	return "Project " + strings.Join(parts, ", "), []Node{p.Input}
}

// SortKey is one ORDER BY term.
type SortKey struct {
	Expr expr.Expr
	Desc bool
}

// Sort materializes its input. Nothing here spills to disk: a sort that does
// not fit in memory is a real limitation, and pretending otherwise by silently
// truncating would be worse than the honest failure.
type Sort struct {
	Input Node
	Keys  []SortKey
}

func (s *Sort) Columns() []Col { return s.Input.Columns() }
func (s *Sort) explain() (string, []Node) {
	parts := make([]string, len(s.Keys))
	for i, k := range s.Keys {
		parts[i] = k.Expr.String()
		if k.Desc {
			parts[i] += " DESC"
		}
	}
	return "Sort " + strings.Join(parts, ", "), []Node{s.Input}
}

// Limit implements LIMIT/OFFSET. Count of -1 means no limit.
type Limit struct {
	Input  Node
	Count  int64
	Offset int64
}

func (l *Limit) Columns() []Col { return l.Input.Columns() }
func (l *Limit) explain() (string, []Node) {
	line := "Limit"
	if l.Count >= 0 {
		line += fmt.Sprintf(" %d", l.Count)
	}
	if l.Offset > 0 {
		line += fmt.Sprintf(" offset %d", l.Offset)
	}
	return line, []Node{l.Input}
}

// Distinct removes duplicate output tuples.
type Distinct struct{ Input Node }

func (d *Distinct) Columns() []Col            { return d.Input.Columns() }
func (d *Distinct) explain() (string, []Node) { return "Distinct", []Node{d.Input} }

// Join is an equi-join executed as a hash join when Keys are present, and as a
// nested loop otherwise. The distinction matters enough to show in EXPLAIN:
// a nested loop over two large tables is the difference between a query that
// returns and one that does not.
type Join struct {
	Left, Right Node
	Type        sql.JoinType
	LeftKeys    []expr.Expr
	RightKeys   []expr.Expr
	Extra       expr.Expr // residual ON predicate
	Out         []Col
}

func (j *Join) Columns() []Col { return j.Out }
func (j *Join) explain() (string, []Node) {
	kind := "HashJoin"
	if len(j.LeftKeys) == 0 {
		kind = "NestedLoopJoin"
	}
	switch j.Type {
	case sql.LeftJoin:
		kind = "Left" + kind
	case sql.CrossJoin:
		kind = "CrossJoin"
	}
	line := kind
	if len(j.LeftKeys) > 0 {
		parts := make([]string, len(j.LeftKeys))
		for i := range j.LeftKeys {
			parts[i] = fmt.Sprintf("%s = %s", j.LeftKeys[i], j.RightKeys[i])
		}
		line += " on " + strings.Join(parts, " AND ")
	}
	if j.Extra != nil {
		line += " residual " + j.Extra.String()
	}
	return line, []Node{j.Left, j.Right}
}

// AggKind is one of the five aggregate functions.
type AggKind uint8

const (
	AggCount AggKind = iota
	AggSum
	AggAvg
	AggMin
	AggMax
)

func (k AggKind) String() string {
	return [...]string{"COUNT", "SUM", "AVG", "MIN", "MAX"}[k]
}

// AggSpec is one aggregate in the select list.
type AggSpec struct {
	Kind AggKind
	Arg  expr.Expr // nil for COUNT(*)
	Name string
	Type types.Type
}

// Aggregate groups its input and computes aggregates. Its output tuple is the
// group keys followed by the aggregate results, in that order, which is the
// layout the select list is bound against.
type Aggregate struct {
	Input   Node
	GroupBy []expr.Expr
	Aggs    []AggSpec
	Out     []Col
}

func (a *Aggregate) Columns() []Col { return a.Out }
func (a *Aggregate) explain() (string, []Node) {
	var parts []string
	for _, g := range a.GroupBy {
		parts = append(parts, "group "+g.String())
	}
	for _, ag := range a.Aggs {
		parts = append(parts, ag.Name)
	}
	return "Aggregate " + strings.Join(parts, ", "), []Node{a.Input}
}

// Values is a literal row source, used by INSERT.
type Values struct {
	Rows [][]expr.Expr
	Out  []Col
}

func (v *Values) Columns() []Col { return v.Out }
func (v *Values) explain() (string, []Node) {
	return fmt.Sprintf("Values (%d rows)", len(v.Rows)), nil
}

// Insert writes its input into a table. Positions maps each input column to a
// position in the table's tuple; unmapped columns take NULL or the row id.
type Insert struct {
	Table     *catalog.Table
	Input     Node
	Positions []int
}

func (i *Insert) Columns() []Col { return nil }
func (i *Insert) explain() (string, []Node) {
	return "Insert into " + i.Table.Name, []Node{i.Input}
}

// Assignment is one SET clause of an UPDATE.
type Assignment struct {
	Pos  int
	Expr expr.Expr
}

// Update rewrites rows produced by its input scan.
type Update struct {
	Table *catalog.Table
	Input *Scan
	Set   []Assignment
}

func (u *Update) Columns() []Col { return nil }
func (u *Update) explain() (string, []Node) {
	return "Update " + u.Table.Name, []Node{u.Input}
}

// Delete removes rows produced by its input scan.
type Delete struct {
	Table *catalog.Table
	Input *Scan
}

func (d *Delete) Columns() []Col            { return nil }
func (d *Delete) explain() (string, []Node) { return "Delete from " + d.Table.Name, []Node{d.Input} }

// DDL nodes carry the parsed statement through to the executor, which is the
// one place allowed to change the catalog.
type CreateTable struct{ Stmt *sql.CreateTable }
type DropTable struct{ Stmt *sql.DropTable }
type CreateIndex struct {
	Stmt  *sql.CreateIndex
	Table *catalog.Table
}

func (c *CreateTable) Columns() []Col { return nil }
func (c *CreateTable) explain() (string, []Node) {
	return "CreateTable " + c.Stmt.Name, nil
}
func (d *DropTable) Columns() []Col            { return nil }
func (d *DropTable) explain() (string, []Node) { return "DropTable " + d.Stmt.Name, nil }
func (c *CreateIndex) Columns() []Col          { return nil }
func (c *CreateIndex) explain() (string, []Node) {
	return fmt.Sprintf("CreateIndex %s on %s", c.Stmt.Name, c.Stmt.Table), nil
}

// Explain renders a plan tree.
func Explain(n Node) string {
	var sb strings.Builder
	writeExplain(&sb, n, 0)
	return sb.String()
}

func writeExplain(sb *strings.Builder, n Node, depth int) {
	line, children := n.explain()
	sb.WriteString(strings.Repeat("  ", depth))
	if depth > 0 {
		sb.WriteString("-> ")
	}
	sb.WriteString(line)
	sb.WriteString("\n")
	for _, c := range children {
		writeExplain(sb, c, depth+1)
	}
}
