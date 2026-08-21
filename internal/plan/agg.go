package plan

import (
	"fmt"

	"github.com/dhananjaypesu/quarry/internal/expr"
	"github.com/dhananjaypesu/quarry/internal/sql"
	"github.com/dhananjaypesu/quarry/internal/types"
)

var aggKinds = map[string]AggKind{
	"COUNT": AggCount,
	"SUM":   AggSum,
	"AVG":   AggAvg,
	"MIN":   AggMin,
	"MAX":   AggMax,
}

func anyAggregate(items []sql.SelectItem) bool {
	for _, it := range items {
		if containsAggregate(it.Expr) {
			return true
		}
	}
	return false
}

func containsAggregate(e sql.Expr) bool {
	switch t := e.(type) {
	case *sql.FuncCall:
		if _, ok := aggKinds[t.Name]; ok {
			return true
		}
		for _, a := range t.Args {
			if containsAggregate(a) {
				return true
			}
		}
	case *sql.BinaryExpr:
		return containsAggregate(t.Left) || containsAggregate(t.Right)
	case *sql.UnaryExpr:
		return containsAggregate(t.X)
	case *sql.IsNullExpr:
		return containsAggregate(t.X)
	case *sql.InExpr:
		if containsAggregate(t.X) {
			return true
		}
		for _, item := range t.List {
			if containsAggregate(item) {
				return true
			}
		}
	case *sql.BetweenExpr:
		return containsAggregate(t.X) || containsAggregate(t.Lo) || containsAggregate(t.Hi)
	}
	return false
}

// aggBuilder rewrites the expressions of a grouped query so that they read
// from the aggregate's output tuple instead of from its input.
//
// The aggregate produces one tuple per group: the group keys first, then one
// slot per distinct aggregate call. Rewriting therefore replaces a group key
// expression with a reference to its slot and an aggregate call with a
// reference to its slot, and rejects anything else that reads a column —
// which is precisely the rule that a non-aggregated column must appear in
// GROUP BY.
type aggBuilder struct {
	b    *Builder
	sc   *scope
	keys map[string]int // bound group expression -> slot

	groupExprs []expr.Expr
	groupCols  []Col
	aggs       []AggSpec
	aggCols    []Col
	seen       map[string]int // aggregate call -> slot, so COUNT(*) twice is computed once
}

func (ab *aggBuilder) rewrite(e sql.Expr) (expr.Expr, error) {
	// An expression that binds cleanly against the input and matches a group
	// key is replaced wholesale, which handles both a bare grouped column and
	// a grouped compound expression such as GROUP BY a + b.
	if bound, err := ab.b.bindExpr(e, ab.sc); err == nil {
		if slot, ok := ab.keys[bound.String()]; ok {
			return &expr.Column{Pos: slot, Typ: bound.Type(), Name: bound.String()}, nil
		}
	}

	switch t := e.(type) {
	case *sql.Literal:
		return &expr.Const{Val: t.Value}, nil
	case *sql.FuncCall:
		return ab.rewriteAgg(t)
	case *sql.ColumnRef:
		name := t.Name
		if t.Table != "" {
			name = t.Table + "." + name
		}
		if _, _, err := ab.sc.resolve(t.Table, t.Name); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("column %s must appear in GROUP BY or be used in an aggregate", name)
	case *sql.BinaryExpr:
		l, err := ab.rewrite(t.Left)
		if err != nil {
			return nil, err
		}
		r, err := ab.rewrite(t.Right)
		if err != nil {
			return nil, err
		}
		return makeBinary(t.Op, l, r)
	case *sql.UnaryExpr:
		x, err := ab.rewrite(t.X)
		if err != nil {
			return nil, err
		}
		if t.Op == "NOT" {
			return &expr.Unary{Op: "NOT", X: x, Typ: types.Bool}, nil
		}
		return &expr.Unary{Op: t.Op, X: x, Typ: x.Type()}, nil
	case *sql.IsNullExpr:
		x, err := ab.rewrite(t.X)
		if err != nil {
			return nil, err
		}
		return &expr.IsNull{X: x, Negate: t.Negate}, nil
	case *sql.InExpr:
		x, err := ab.rewrite(t.X)
		if err != nil {
			return nil, err
		}
		out := &expr.In{X: x, Negate: t.Negate}
		for _, item := range t.List {
			v, err := ab.rewrite(item)
			if err != nil {
				return nil, err
			}
			out.List = append(out.List, v)
		}
		return out, nil
	case *sql.BetweenExpr:
		x, err := ab.rewrite(t.X)
		if err != nil {
			return nil, err
		}
		lo, err := ab.rewrite(t.Lo)
		if err != nil {
			return nil, err
		}
		hi, err := ab.rewrite(t.Hi)
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
	}
	return nil, fmt.Errorf("cannot use expression %T in a grouped query", e)
}

func (ab *aggBuilder) rewriteAgg(fc *sql.FuncCall) (expr.Expr, error) {
	kind, ok := aggKinds[fc.Name]
	if !ok {
		return nil, fmt.Errorf("unknown function %s", fc.Name)
	}
	spec := AggSpec{Kind: kind}
	switch {
	case fc.Star:
		if kind != AggCount {
			return nil, fmt.Errorf("%s(*) is not defined", fc.Name)
		}
		spec.Name = "COUNT(*)"
		spec.Type = types.Int
	case len(fc.Args) == 1:
		arg, err := ab.b.bindExpr(fc.Args[0], ab.sc)
		if err != nil {
			return nil, err
		}
		if containsAggregate(fc.Args[0]) {
			return nil, fmt.Errorf("aggregate functions cannot be nested")
		}
		spec.Arg = arg
		spec.Name = fmt.Sprintf("%s(%s)", fc.Name, arg)
		spec.Type = aggResultType(kind, arg.Type())
		if spec.Type == types.Unknown {
			return nil, fmt.Errorf("%s is not defined on %s", fc.Name, arg.Type())
		}
	default:
		return nil, fmt.Errorf("%s takes exactly one argument", fc.Name)
	}

	if ab.seen == nil {
		ab.seen = map[string]int{}
	}
	if slot, ok := ab.seen[spec.Name]; ok {
		return &expr.Column{Pos: len(ab.groupExprs) + slot, Typ: spec.Type, Name: spec.Name}, nil
	}
	slot := len(ab.aggs)
	ab.seen[spec.Name] = slot
	ab.aggs = append(ab.aggs, spec)
	ab.aggCols = append(ab.aggCols, Col{Name: spec.Name, Type: spec.Type})
	return &expr.Column{Pos: len(ab.groupExprs) + slot, Typ: spec.Type, Name: spec.Name}, nil
}

func aggResultType(k AggKind, arg types.Type) types.Type {
	switch k {
	case AggCount:
		return types.Int
	case AggAvg:
		if arg == types.Int || arg == types.Float {
			return types.Float
		}
	case AggSum:
		if arg == types.Int || arg == types.Float {
			return arg
		}
	case AggMin, AggMax:
		return arg
	}
	return types.Unknown
}
