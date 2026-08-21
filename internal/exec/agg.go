package exec

import (
	"github.com/dhananjaypesu/quarry/internal/expr"
	"github.com/dhananjaypesu/quarry/internal/plan"
	"github.com/dhananjaypesu/quarry/internal/txn"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// aggregateOp groups rows in a hash table and folds each group down to one
// tuple of (group keys..., aggregate results...).
//
// Groups are emitted in the order they were first seen rather than in hash
// order. Nothing in SQL requires that, but a stable order makes results
// reproducible, which matters more for a test suite than the microseconds it
// costs to keep a slice of keys alongside the map.
type aggregateOp struct {
	in      Operator
	groupBy []expr.Expr
	specs   []plan.AggSpec

	order  []string
	groups map[string]*group
	done   bool
	pos    int
}

type group struct {
	keys []types.Value
	accs []accumulator
}

type accumulator struct {
	count int64
	sum   types.Value
	best  types.Value
	seen  bool
}

func newAggregate(tx *txn.Txn, p *plan.Aggregate) (Operator, error) {
	in, err := Build(tx, p.Input)
	if err != nil {
		return nil, err
	}
	return &aggregateOp{in: in, groupBy: p.GroupBy, specs: p.Aggs, groups: map[string]*group{}}, nil
}

func (a *aggregateOp) Next() (row, bool, error) {
	if !a.done {
		if err := a.consume(); err != nil {
			return nil, false, err
		}
	}
	if a.pos >= len(a.order) {
		return nil, false, nil
	}
	g := a.groups[a.order[a.pos]]
	a.pos++
	out := append(row(nil), g.keys...)
	for i, spec := range a.specs {
		out = append(out, g.accs[i].result(spec))
	}
	return out, true, nil
}

func (a *aggregateOp) consume() error {
	a.done = true
	for {
		r, ok, err := a.in.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		keys := make([]types.Value, len(a.groupBy))
		for i, e := range a.groupBy {
			if keys[i], err = e.Eval(r); err != nil {
				return err
			}
		}
		k := keyOf(keys)
		g, ok := a.groups[k]
		if !ok {
			g = &group{keys: keys, accs: make([]accumulator, len(a.specs))}
			a.groups[k] = g
			a.order = append(a.order, k)
		}
		for i, spec := range a.specs {
			if err := g.accs[i].add(spec, r); err != nil {
				return err
			}
		}
	}
	// An ungrouped aggregate over an empty input still has an answer:
	// COUNT(*) is 0 and SUM is NULL. Only a GROUP BY query returns no rows.
	if len(a.groupBy) == 0 && len(a.order) == 0 {
		a.groups[""] = &group{accs: make([]accumulator, len(a.specs))}
		a.order = append(a.order, "")
	}
	return nil
}

func (acc *accumulator) add(spec plan.AggSpec, r row) error {
	if spec.Arg == nil { // COUNT(*)
		acc.count++
		return nil
	}
	v, err := spec.Arg.Eval(r)
	if err != nil {
		return err
	}
	// Every aggregate but COUNT(*) ignores NULL inputs, which is why COUNT(x)
	// and COUNT(*) can differ.
	if v.Null {
		return nil
	}
	acc.count++
	switch spec.Kind {
	case plan.AggSum, plan.AggAvg:
		if !acc.seen {
			acc.sum, acc.seen = v, true
			return nil
		}
		acc.sum = addValues(acc.sum, v)
	case plan.AggMin, plan.AggMax:
		if !acc.seen {
			acc.best, acc.seen = v, true
			return nil
		}
		c, ok := types.Compare(v, acc.best)
		if !ok {
			return nil
		}
		if (spec.Kind == plan.AggMin && c < 0) || (spec.Kind == plan.AggMax && c > 0) {
			acc.best = v
		}
	}
	return nil
}

func (acc *accumulator) result(spec plan.AggSpec) types.Value {
	switch spec.Kind {
	case plan.AggCount:
		return types.NewInt(acc.count)
	case plan.AggSum:
		if !acc.seen {
			return types.NullOf(spec.Type)
		}
		return acc.sum
	case plan.AggAvg:
		if !acc.seen || acc.count == 0 {
			return types.NullOf(types.Float)
		}
		return types.NewFloat(floatOf(acc.sum) / float64(acc.count))
	case plan.AggMin, plan.AggMax:
		if !acc.seen {
			return types.NullOf(spec.Type)
		}
		return acc.best
	}
	return types.NullValue
}

func addValues(a, b types.Value) types.Value {
	if a.Type == types.Float || b.Type == types.Float {
		return types.NewFloat(floatOf(a) + floatOf(b))
	}
	return types.NewInt(a.I + b.I)
}

func floatOf(v types.Value) float64 {
	if v.Type == types.Int {
		return float64(v.I)
	}
	return v.F
}

func (a *aggregateOp) Close() error { return a.in.Close() }
