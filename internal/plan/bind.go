package plan

import (
	"fmt"
	"strings"

	"github.com/dhananjaypesu/quarry/internal/catalog"
	"github.com/dhananjaypesu/quarry/internal/expr"
	"github.com/dhananjaypesu/quarry/internal/sql"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// Builder binds a statement against the catalog.
type Builder struct{ cat *catalog.Catalog }

// NewBuilder returns a builder reading schemas from cat.
func NewBuilder(cat *catalog.Catalog) *Builder { return &Builder{cat: cat} }

// scope is the set of columns visible to an expression, in tuple order. Name
// resolution walks it linearly: a query joins a handful of tables, so a map
// would cost more to build than the scan saves.
type scope struct{ cols []Col }

func (s *scope) resolve(table, name string) (int, Col, error) {
	found := -1
	for i, c := range s.cols {
		if !strings.EqualFold(c.Name, name) {
			continue
		}
		if table != "" && !strings.EqualFold(c.Table, table) {
			continue
		}
		if found >= 0 {
			return 0, Col{}, fmt.Errorf("column reference %q is ambiguous", name)
		}
		found = i
	}
	if found < 0 {
		if table != "" {
			return 0, Col{}, fmt.Errorf("no column %s.%s in scope", table, name)
		}
		return 0, Col{}, fmt.Errorf("no column %q in scope", name)
	}
	return found, s.cols[found], nil
}

// Build produces a plan for a statement.
func (b *Builder) Build(st sql.Statement) (Node, error) {
	switch s := st.(type) {
	case *sql.Select:
		return b.buildSelect(s)
	case *sql.Insert:
		return b.buildInsert(s)
	case *sql.Update:
		return b.buildUpdate(s)
	case *sql.Delete:
		return b.buildDelete(s)
	case *sql.CreateTable:
		return &CreateTable{Stmt: s}, nil
	case *sql.DropTable:
		return &DropTable{Stmt: s}, nil
	case *sql.CreateIndex:
		t, ok := b.cat.Get(s.Table)
		if !ok {
			return nil, fmt.Errorf("no such table: %s", s.Table)
		}
		return &CreateIndex{Stmt: s, Table: t}, nil
	}
	return nil, fmt.Errorf("statement cannot be planned: %T", st)
}

// ---- FROM ----

// scanSite records where a base table's columns land in the joined tuple, so
// that a predicate over only those columns can be pushed into its scan.
type scanSite struct {
	scan     *Scan
	offset   int
	width    int
	pushable bool
	pushed   []expr.Expr
}

type fromResult struct {
	node  Node
	sc    *scope
	sites []*scanSite
}

func (b *Builder) buildFrom(ref *sql.TableRef) (*fromResult, error) {
	base, err := b.buildTableRef(*ref)
	if err != nil {
		return nil, err
	}
	res := &fromResult{node: base.scan, sc: &scope{cols: base.scan.Out}, sites: []*scanSite{base}}

	for _, jc := range ref.Joins {
		right, err := b.buildTableRef(jc.Right)
		if err != nil {
			return nil, err
		}
		offset := len(res.sc.cols)
		right.offset = offset
		// A LEFT JOIN manufactures NULL rows for unmatched left rows, so a
		// predicate pushed into the right scan would remove rows that the join
		// is required to preserve as NULL-extended. Only the outer side stays
		// pushable.
		if jc.Type == sql.LeftJoin {
			right.pushable = false
		}
		joined := &scope{cols: append(append([]Col{}, res.sc.cols...), right.scan.Out...)}

		join := &Join{Left: res.node, Right: right.scan, Type: jc.Type, Out: joined.cols}
		if jc.On != nil {
			on, err := b.bindExpr(jc.On, joined)
			if err != nil {
				return nil, err
			}
			lk, rk, residual := splitJoinKeys(splitConjuncts(on), offset)
			join.LeftKeys, join.RightKeys, join.Extra = lk, rk, combine(residual)
		}
		res.node = join
		res.sc = joined
		res.sites = append(res.sites, right)
	}
	return res, nil
}

func (b *Builder) buildTableRef(ref sql.TableRef) (*scanSite, error) {
	t, ok := b.cat.Get(ref.Name)
	if !ok {
		return nil, fmt.Errorf("no such table: %s", ref.Name)
	}
	alias := ref.Alias
	if alias == "" {
		alias = t.Name
	}
	out := make([]Col, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = Col{Table: alias, Name: c.Name, Type: c.Type}
	}
	// Start, End and Access are filled in by finalize once every predicate
	// that could narrow the scan has been pushed down.
	scan := &Scan{Table: t, Alias: alias, Out: out}
	return &scanSite{scan: scan, width: len(out), pushable: true}, nil
}

// splitJoinKeys pulls equi-join conjuncts out of an ON clause. A conjunct
// qualifies when it is an equality with one side reading only left columns and
// the other only right columns; everything else stays as a residual predicate
// evaluated on the joined row.
func splitJoinKeys(conjuncts []expr.Expr, offset int) (left, right []expr.Expr, residual []expr.Expr) {
	for _, c := range conjuncts {
		bin, ok := c.(*expr.Binary)
		if !ok || bin.Op != "=" {
			residual = append(residual, c)
			continue
		}
		lMin, lMax, lOK := refRange(bin.L)
		rMin, rMax, rOK := refRange(bin.R)
		switch {
		case lOK && rOK && lMax < offset && rMin >= offset:
			left = append(left, bin.L)
			right = append(right, shift(bin.R, -offset))
		case lOK && rOK && rMax < offset && lMin >= offset:
			left = append(left, bin.R)
			right = append(right, shift(bin.L, -offset))
		default:
			residual = append(residual, c)
		}
	}
	return left, right, residual
}

// pushDown moves each WHERE conjunct into the scan whose columns it reads,
// leaving the rest to be evaluated above the join.
func pushDown(sites []*scanSite, conjuncts []expr.Expr) []expr.Expr {
	var remaining []expr.Expr
	for _, c := range conjuncts {
		min, max, ok := refRange(c)
		placed := false
		if ok {
			for _, s := range sites {
				if !s.pushable || min < s.offset || max >= s.offset+s.width {
					continue
				}
				s.pushed = append(s.pushed, shift(c, -s.offset))
				placed = true
				break
			}
		}
		if !placed {
			remaining = append(remaining, c)
		}
	}
	return remaining
}

// finalize chooses each scan's access path once all pushed predicates are known.
func finalize(sites []*scanSite) {
	for _, s := range sites {
		chosen := choosePath(s.scan.Table, s.pushed)
		s.scan.Index = chosen.Index
		s.scan.Start = chosen.Start
		s.scan.End = chosen.End
		s.scan.Access = chosen.Access
		s.scan.Filter = chosen.Filter
	}
}

// ---- SELECT ----

func (b *Builder) buildSelect(st *sql.Select) (Node, error) {
	if st.From == nil {
		return nil, fmt.Errorf("SELECT without FROM is not supported")
	}
	from, err := b.buildFrom(st.From)
	if err != nil {
		return nil, err
	}
	node, sc := from.node, from.sc

	var afterJoin []expr.Expr
	if st.Where != nil {
		pred, err := b.bindExpr(st.Where, sc)
		if err != nil {
			return nil, err
		}
		if pred.Type() != types.Bool && pred.Type() != types.Unknown {
			return nil, fmt.Errorf("WHERE requires a boolean expression, found %s", pred.Type())
		}
		afterJoin = pushDown(from.sites, splitConjuncts(pred))
	}
	finalize(from.sites)
	if len(afterJoin) > 0 {
		node = &Filter{Input: node, Pred: combine(afterJoin)}
	}

	items, err := expandStars(st.Items, sc)
	if err != nil {
		return nil, err
	}

	grouped := len(st.GroupBy) > 0 || st.Having != nil || anyAggregate(items)
	var selExprs []expr.Expr
	var outCols []Col
	var having expr.Expr
	var orderKeys []SortKey

	if grouped {
		ab := &aggBuilder{b: b, sc: sc, keys: map[string]int{}}
		for _, g := range st.GroupBy {
			bound, err := b.bindExpr(g, sc)
			if err != nil {
				return nil, err
			}
			ab.keys[bound.String()] = len(ab.groupExprs)
			ab.groupExprs = append(ab.groupExprs, bound)
			ab.groupCols = append(ab.groupCols, Col{Name: bound.String(), Type: bound.Type()})
		}
		aliases := map[string]expr.Expr{}
		for _, item := range items {
			e, err := ab.rewrite(item.Expr)
			if err != nil {
				return nil, err
			}
			selExprs = append(selExprs, e)
			outCols = append(outCols, Col{Name: itemName(item, e), Type: e.Type()})
			if item.Alias != "" {
				aliases[strings.ToLower(item.Alias)] = e
			}
		}
		if st.Having != nil {
			if having, err = ab.rewrite(st.Having); err != nil {
				return nil, err
			}
		}
		for _, o := range st.OrderBy {
			if e, ok := aliasRef(o.Expr, aliases); ok {
				orderKeys = append(orderKeys, SortKey{Expr: e, Desc: o.Desc})
				continue
			}
			e, err := ab.rewrite(o.Expr)
			if err != nil {
				return nil, err
			}
			orderKeys = append(orderKeys, SortKey{Expr: e, Desc: o.Desc})
		}

		agg := &Aggregate{Input: node, GroupBy: ab.groupExprs, Aggs: ab.aggs}
		agg.Out = append(append([]Col{}, ab.groupCols...), ab.aggCols...)
		node = agg
		if having != nil {
			node = &Filter{Input: node, Pred: having}
		}
	} else {
		aliases := map[string]expr.Expr{}
		for _, item := range items {
			e, err := b.bindExpr(item.Expr, sc)
			if err != nil {
				return nil, err
			}
			selExprs = append(selExprs, e)
			name := itemName(item, e)
			outCols = append(outCols, Col{Name: name, Type: e.Type()})
			if item.Alias != "" {
				aliases[strings.ToLower(item.Alias)] = e
			}
		}
		for _, o := range st.OrderBy {
			if e, ok := aliasRef(o.Expr, aliases); ok {
				orderKeys = append(orderKeys, SortKey{Expr: e, Desc: o.Desc})
				continue
			}
			e, err := b.bindExpr(o.Expr, sc)
			if err != nil {
				return nil, err
			}
			orderKeys = append(orderKeys, SortKey{Expr: e, Desc: o.Desc})
		}
	}

	// Sorting happens before projection because ORDER BY may reference
	// columns the select list does not carry forward.
	if len(orderKeys) > 0 {
		node = &Sort{Input: node, Keys: orderKeys}
	}
	node = &Project{Input: node, Exprs: selExprs, Out: outCols}
	if st.Distinct {
		node = &Distinct{Input: node}
	}
	if st.Limit != nil || st.Offset != nil {
		count, err := constInt(st.Limit, -1)
		if err != nil {
			return nil, fmt.Errorf("LIMIT: %w", err)
		}
		offset, err := constInt(st.Offset, 0)
		if err != nil {
			return nil, fmt.Errorf("OFFSET: %w", err)
		}
		node = &Limit{Input: node, Count: count, Offset: offset}
	}
	return node, nil
}

// aliasRef resolves an ORDER BY term that names an output alias rather than an
// input column, which is what makes "SELECT a + b AS s ... ORDER BY s" work.
// In a grouped query it is also the only way to sort by an aggregate without
// repeating the call.
func aliasRef(e sql.Expr, aliases map[string]expr.Expr) (expr.Expr, bool) {
	ref, ok := e.(*sql.ColumnRef)
	if !ok || ref.Table != "" {
		return nil, false
	}
	bound, ok := aliases[strings.ToLower(ref.Name)]
	return bound, ok
}

func itemName(item sql.SelectItem, e expr.Expr) string {
	if item.Alias != "" {
		return item.Alias
	}
	return e.String()
}

func constInt(e sql.Expr, def int64) (int64, error) {
	if e == nil {
		return def, nil
	}
	lit, ok := e.(*sql.Literal)
	if !ok || lit.Value.Type != types.Int {
		return 0, fmt.Errorf("expected an integer constant")
	}
	if lit.Value.I < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return lit.Value.I, nil
}

// expandStars replaces * and t.* with the columns they stand for. Hidden
// columns are skipped: the row id exists to give the storage layer a key, not
// to appear in results.
func expandStars(items []sql.SelectItem, sc *scope) ([]sql.SelectItem, error) {
	var out []sql.SelectItem
	for _, item := range items {
		switch {
		case item.Star:
			for _, c := range sc.cols {
				if strings.EqualFold(c.Name, catalog.RowIDColumn) {
					continue
				}
				out = append(out, sql.SelectItem{Expr: &sql.ColumnRef{Table: c.Table, Name: c.Name}, Alias: c.Name})
			}
		case item.TableStar != "":
			n := 0
			for _, c := range sc.cols {
				if !strings.EqualFold(c.Table, item.TableStar) || strings.EqualFold(c.Name, catalog.RowIDColumn) {
					continue
				}
				out = append(out, sql.SelectItem{Expr: &sql.ColumnRef{Table: c.Table, Name: c.Name}, Alias: c.Name})
				n++
			}
			if n == 0 {
				return nil, fmt.Errorf("no table %q in FROM clause", item.TableStar)
			}
		default:
			out = append(out, item)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("select list is empty")
	}
	return out, nil
}

// ---- INSERT / UPDATE / DELETE ----

func (b *Builder) buildInsert(st *sql.Insert) (Node, error) {
	t, ok := b.cat.Get(st.Table)
	if !ok {
		return nil, fmt.Errorf("no such table: %s", st.Table)
	}
	var targets []int
	if len(st.Columns) == 0 {
		for i, c := range t.Columns {
			if c.Hidden {
				continue
			}
			targets = append(targets, i)
		}
	} else {
		seen := map[int]bool{}
		for _, name := range st.Columns {
			col, pos, ok := t.ColumnByName(name)
			if !ok || col.Hidden {
				return nil, fmt.Errorf("no column %q in table %s", name, t.Name)
			}
			if seen[pos] {
				return nil, fmt.Errorf("column %q appears twice in INSERT", name)
			}
			seen[pos] = true
			targets = append(targets, pos)
		}
	}

	empty := &scope{}
	values := &Values{}
	for _, row := range st.Rows {
		if len(row) != len(targets) {
			return nil, fmt.Errorf("INSERT has %d columns but %d values", len(targets), len(row))
		}
		bound := make([]expr.Expr, len(row))
		for i, e := range row {
			// The scope is empty on purpose: VALUES may not reference columns,
			// so a stray name is an error here rather than a NULL at runtime.
			v, err := b.bindExpr(e, empty)
			if err != nil {
				return nil, err
			}
			bound[i] = v
		}
		values.Rows = append(values.Rows, bound)
	}
	for _, pos := range targets {
		values.Out = append(values.Out, Col{Table: t.Name, Name: t.Columns[pos].Name, Type: t.Columns[pos].Type})
	}
	return &Insert{Table: t, Input: values, Positions: targets}, nil
}

func (b *Builder) buildUpdate(st *sql.Update) (Node, error) {
	t, sc, scan, err := b.scanFor(st.Table, st.Where)
	if err != nil {
		return nil, err
	}
	var sets []Assignment
	for _, a := range st.Set {
		col, pos, ok := t.ColumnByName(a.Column)
		if !ok || col.Hidden {
			return nil, fmt.Errorf("no column %q in table %s", a.Column, t.Name)
		}
		e, err := b.bindExpr(a.Value, sc)
		if err != nil {
			return nil, err
		}
		sets = append(sets, Assignment{Pos: pos, Expr: e})
	}
	return &Update{Table: t, Input: scan, Set: sets}, nil
}

func (b *Builder) buildDelete(st *sql.Delete) (Node, error) {
	t, _, scan, err := b.scanFor(st.Table, st.Where)
	if err != nil {
		return nil, err
	}
	return &Delete{Table: t, Input: scan}, nil
}

// scanFor builds the single-table scan that UPDATE and DELETE mutate through.
func (b *Builder) scanFor(name string, where sql.Expr) (*catalog.Table, *scope, *Scan, error) {
	site, err := b.buildTableRef(sql.TableRef{Name: name})
	if err != nil {
		return nil, nil, nil, err
	}
	sc := &scope{cols: site.scan.Out}
	if where != nil {
		pred, err := b.bindExpr(where, sc)
		if err != nil {
			return nil, nil, nil, err
		}
		site.pushed = splitConjuncts(pred)
	}
	finalize([]*scanSite{site})
	return site.scan.Table, sc, site.scan, nil
}

// ---- expression binding ----

func (b *Builder) bindExpr(e sql.Expr, sc *scope) (expr.Expr, error) {
	switch t := e.(type) {
	case *sql.Literal:
		return &expr.Const{Val: t.Value}, nil
	case *sql.ColumnRef:
		pos, col, err := sc.resolve(t.Table, t.Name)
		if err != nil {
			return nil, err
		}
		return &expr.Column{Pos: pos, Typ: col.Type, Name: col.Name}, nil
	case *sql.BinaryExpr:
		l, err := b.bindExpr(t.Left, sc)
		if err != nil {
			return nil, err
		}
		r, err := b.bindExpr(t.Right, sc)
		if err != nil {
			return nil, err
		}
		return makeBinary(t.Op, l, r)
	case *sql.UnaryExpr:
		x, err := b.bindExpr(t.X, sc)
		if err != nil {
			return nil, err
		}
		if t.Op == "NOT" {
			return &expr.Unary{Op: "NOT", X: x, Typ: types.Bool}, nil
		}
		if x.Type() != types.Int && x.Type() != types.Float && x.Type() != types.Unknown {
			return nil, fmt.Errorf("cannot negate %s", x.Type())
		}
		return &expr.Unary{Op: t.Op, X: x, Typ: x.Type()}, nil
	case *sql.IsNullExpr:
		x, err := b.bindExpr(t.X, sc)
		if err != nil {
			return nil, err
		}
		return &expr.IsNull{X: x, Negate: t.Negate}, nil
	case *sql.InExpr:
		x, err := b.bindExpr(t.X, sc)
		if err != nil {
			return nil, err
		}
		in := &expr.In{X: x, Negate: t.Negate}
		for _, item := range t.List {
			v, err := b.bindExpr(item, sc)
			if err != nil {
				return nil, err
			}
			if err := checkComparable(x.Type(), v.Type()); err != nil {
				return nil, err
			}
			in.List = append(in.List, v)
		}
		return in, nil
	case *sql.BetweenExpr:
		// BETWEEN is desugared into two comparisons, which is exactly what it
		// means, and lets access-path selection see the two range bounds it
		// already knows how to use.
		x, err := b.bindExpr(t.X, sc)
		if err != nil {
			return nil, err
		}
		lo, err := b.bindExpr(t.Lo, sc)
		if err != nil {
			return nil, err
		}
		hi, err := b.bindExpr(t.Hi, sc)
		if err != nil {
			return nil, err
		}
		low, err := makeBinary(">=", x, lo)
		if err != nil {
			return nil, err
		}
		high, err := makeBinary("<=", x, hi)
		if err != nil {
			return nil, err
		}
		both := &expr.Binary{Op: "AND", L: low, R: high, Typ: types.Bool}
		if t.Negate {
			return &expr.Unary{Op: "NOT", X: both, Typ: types.Bool}, nil
		}
		return both, nil
	case *sql.FuncCall:
		if _, ok := aggKinds[t.Name]; ok {
			return nil, fmt.Errorf("aggregate %s is not allowed here", t.Name)
		}
		return nil, fmt.Errorf("unknown function %s", t.Name)
	}
	return nil, fmt.Errorf("cannot bind expression %T", e)
}

func makeBinary(op string, l, r expr.Expr) (expr.Expr, error) {
	switch op {
	case "AND", "OR":
		return &expr.Binary{Op: op, L: l, R: r, Typ: types.Bool}, nil
	case "=", "!=", "<", "<=", ">", ">=":
		if err := checkComparable(l.Type(), r.Type()); err != nil {
			return nil, err
		}
		return &expr.Binary{Op: op, L: l, R: r, Typ: types.Bool}, nil
	case "LIKE":
		if !isTextish(l.Type()) || !isTextish(r.Type()) {
			return nil, fmt.Errorf("LIKE requires text operands")
		}
		return &expr.Binary{Op: op, L: l, R: r, Typ: types.Bool}, nil
	case "||":
		return &expr.Binary{Op: op, L: l, R: r, Typ: types.Text}, nil
	case "+", "-", "*", "/", "%":
		lt, rt := l.Type(), r.Type()
		if lt == types.Unknown || rt == types.Unknown {
			return &expr.Binary{Op: op, L: l, R: r, Typ: numericType(lt, rt)}, nil
		}
		if !isNumeric(lt) || !isNumeric(rt) {
			return nil, fmt.Errorf("operator %s is not defined on %s and %s", op, lt, rt)
		}
		return &expr.Binary{Op: op, L: l, R: r, Typ: numericType(lt, rt)}, nil
	}
	return nil, fmt.Errorf("unsupported operator %q", op)
}

func numericType(a, b types.Type) types.Type {
	if a == types.Float || b == types.Float {
		return types.Float
	}
	return types.Int
}

func isNumeric(t types.Type) bool { return t == types.Int || t == types.Float }
func isTextish(t types.Type) bool { return t == types.Text || t == types.Unknown }

// checkComparable rejects comparisons that could never be true, so that a
// mistyped literal fails at plan time instead of quietly matching nothing.
func checkComparable(a, b types.Type) error {
	if a == types.Unknown || b == types.Unknown || a == b {
		return nil
	}
	if isNumeric(a) && isNumeric(b) {
		return nil
	}
	return fmt.Errorf("cannot compare %s with %s", a, b)
}

// ---- expression utilities ----

// refRange reports the lowest and highest column positions an expression
// reads. It is how the planner decides which scan a predicate belongs to.
func refRange(e expr.Expr) (min, max int, ok bool) {
	min, max = 1<<31, -1
	visit(e, func(pos int) {
		if pos < min {
			min = pos
		}
		if pos > max {
			max = pos
		}
	})
	return min, max, max >= 0
}

func visit(e expr.Expr, f func(pos int)) {
	switch t := e.(type) {
	case *expr.Column:
		f(t.Pos)
	case *expr.Binary:
		visit(t.L, f)
		visit(t.R, f)
	case *expr.Unary:
		visit(t.X, f)
	case *expr.IsNull:
		visit(t.X, f)
	case *expr.In:
		visit(t.X, f)
		for _, item := range t.List {
			visit(item, f)
		}
	}
}

// shift rebases column positions, which is what lets a predicate bound against
// a joined tuple be evaluated against one side of the join on its own.
func shift(e expr.Expr, delta int) expr.Expr {
	switch t := e.(type) {
	case *expr.Column:
		c := *t
		c.Pos += delta
		return &c
	case *expr.Binary:
		return &expr.Binary{Op: t.Op, L: shift(t.L, delta), R: shift(t.R, delta), Typ: t.Typ}
	case *expr.Unary:
		return &expr.Unary{Op: t.Op, X: shift(t.X, delta), Typ: t.Typ}
	case *expr.IsNull:
		return &expr.IsNull{X: shift(t.X, delta), Negate: t.Negate}
	case *expr.In:
		out := &expr.In{X: shift(t.X, delta), Negate: t.Negate}
		for _, item := range t.List {
			out.List = append(out.List, shift(item, delta))
		}
		return out
	}
	return e
}
