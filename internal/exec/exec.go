package exec

import (
	"fmt"
	"sort"

	"github.com/dhananjaypesu/quarry/internal/catalog"
	"github.com/dhananjaypesu/quarry/internal/encoding"
	"github.com/dhananjaypesu/quarry/internal/expr"
	"github.com/dhananjaypesu/quarry/internal/plan"
	"github.com/dhananjaypesu/quarry/internal/txn"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// Operator is a pull-based iterator over tuples.
type Operator interface {
	// Next returns the next tuple, or ok=false when the input is exhausted.
	Next() (row, bool, error)
	Close() error
}

// Build compiles a query plan into an operator tree.
func Build(tx *txn.Txn, n plan.Node) (Operator, error) {
	switch p := n.(type) {
	case *plan.Scan:
		return newScan(tx, p)
	case *plan.Filter:
		in, err := Build(tx, p.Input)
		if err != nil {
			return nil, err
		}
		return &filterOp{in: in, pred: p.Pred}, nil
	case *plan.Project:
		in, err := Build(tx, p.Input)
		if err != nil {
			return nil, err
		}
		return &projectOp{in: in, exprs: p.Exprs}, nil
	case *plan.Sort:
		in, err := Build(tx, p.Input)
		if err != nil {
			return nil, err
		}
		return &sortOp{in: in, keys: p.Keys}, nil
	case *plan.Limit:
		in, err := Build(tx, p.Input)
		if err != nil {
			return nil, err
		}
		return &limitOp{in: in, count: p.Count, offset: p.Offset}, nil
	case *plan.Distinct:
		in, err := Build(tx, p.Input)
		if err != nil {
			return nil, err
		}
		return &distinctOp{in: in, seen: map[string]bool{}}, nil
	case *plan.Join:
		return newJoin(tx, p)
	case *plan.Aggregate:
		return newAggregate(tx, p)
	case *plan.Values:
		return &valuesOp{rows: p.Rows}, nil
	}
	return nil, fmt.Errorf("cannot execute plan node %T", n)
}

// ---- scan ----

// scanOp walks a key range. For a primary-key path it decodes each row
// directly; for an index path it decodes the primary key out of the entry and
// fetches the row, which is the extra hop that makes the planner prefer the
// primary key on a tie.
type scanOp struct {
	tx    *txn.Txn
	table *catalog.Table
	index *catalog.Index
	it    *txn.Iter
	pred  expr.Expr
	first bool
}

func newScan(tx *txn.Txn, p *plan.Scan) (Operator, error) {
	it, err := tx.NewIter(p.Start, p.End)
	if err != nil {
		return nil, err
	}
	return &scanOp{tx: tx, table: p.Table, index: p.Index, it: it, pred: p.Filter, first: true}, nil
}

func (s *scanOp) Next() (row, bool, error) {
	for {
		if !s.first {
			s.it.Next()
		}
		s.first = false
		if !s.it.Valid() {
			return nil, false, s.it.Err()
		}
		r, err := s.materialize()
		if err != nil {
			return nil, false, err
		}
		if r == nil {
			continue
		}
		ok, err := expr.Passes(s.pred, r)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return r, true, nil
		}
	}
}

func (s *scanOp) materialize() (row, error) {
	if s.index == nil {
		return decodeRow(s.table, s.it.Key(), s.it.Value())
	}
	key := append(encoding.TableRowPrefix(s.table.ID), s.it.Value()...)
	val, err := s.tx.Get(key)
	if err != nil {
		// An index entry with no row behind it means the two got out of step,
		// which this engine treats as corruption rather than skipping quietly.
		return nil, fmt.Errorf("index %s points at a missing row: %w", s.index.Name, err)
	}
	return decodeRow(s.table, key, val)
}

func (s *scanOp) Close() error { return s.it.Close() }

// ---- filter, project, limit, distinct ----

type filterOp struct {
	in   Operator
	pred expr.Expr
}

func (f *filterOp) Next() (row, bool, error) {
	for {
		r, ok, err := f.in.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		pass, err := expr.Passes(f.pred, r)
		if err != nil {
			return nil, false, err
		}
		if pass {
			return r, true, nil
		}
	}
}

func (f *filterOp) Close() error { return f.in.Close() }

type projectOp struct {
	in    Operator
	exprs []expr.Expr
}

func (p *projectOp) Next() (row, bool, error) {
	r, ok, err := p.in.Next()
	if err != nil || !ok {
		return nil, false, err
	}
	out := make(row, len(p.exprs))
	for i, e := range p.exprs {
		v, err := e.Eval(r)
		if err != nil {
			return nil, false, err
		}
		out[i] = v
	}
	return out, true, nil
}

func (p *projectOp) Close() error { return p.in.Close() }

type limitOp struct {
	in      Operator
	count   int64
	offset  int64
	emitted int64
	skipped int64
}

func (l *limitOp) Next() (row, bool, error) {
	for {
		if l.count >= 0 && l.emitted >= l.count {
			return nil, false, nil
		}
		r, ok, err := l.in.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		if l.skipped < l.offset {
			l.skipped++
			continue
		}
		l.emitted++
		return r, true, nil
	}
}

func (l *limitOp) Close() error { return l.in.Close() }

// distinctOp remembers every tuple it has emitted, which is the memory cost of
// DISTINCT without a sort. Tuples are keyed by their order-preserving
// encoding, so equality here means exactly what equality means in an index.
type distinctOp struct {
	in   Operator
	seen map[string]bool
}

func (d *distinctOp) Next() (row, bool, error) {
	for {
		r, ok, err := d.in.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		k := string(encoding.EncodeKeyValues(nil, r...))
		if d.seen[k] {
			continue
		}
		d.seen[k] = true
		return r, true, nil
	}
}

func (d *distinctOp) Close() error { return d.in.Close() }

// ---- sort ----

// sortOp buffers its whole input. A merge sort that spills to disk would lift
// that limit, but it would also be the only operator in the engine that owns
// temporary files, so the honest version is the in-memory one that fails
// loudly if the input does not fit.
type sortOp struct {
	in     Operator
	keys   []plan.SortKey
	rows   []row
	pos    int
	loaded bool
}

func (s *sortOp) Next() (row, bool, error) {
	if !s.loaded {
		if err := s.load(); err != nil {
			return nil, false, err
		}
	}
	if s.pos >= len(s.rows) {
		return nil, false, nil
	}
	r := s.rows[s.pos]
	s.pos++
	return r, true, nil
}

func (s *sortOp) load() error {
	s.loaded = true
	// Sort keys are evaluated once per row and cached alongside it, so a
	// comparison-heavy sort does not re-run the expression O(n log n) times.
	type entry struct {
		r    row
		keys []types.Value
	}
	var entries []entry
	for {
		r, ok, err := s.in.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		e := entry{r: append(row(nil), r...)}
		for _, k := range s.keys {
			v, err := k.Expr.Eval(r)
			if err != nil {
				return err
			}
			e.keys = append(e.keys, v)
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		for k := range s.keys {
			c := types.CompareNullsFirst(entries[i].keys[k], entries[j].keys[k])
			if c == 0 {
				continue
			}
			if s.keys[k].Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	s.rows = make([]row, len(entries))
	for i, e := range entries {
		s.rows[i] = e.r
	}
	return nil
}

func (s *sortOp) Close() error { return s.in.Close() }

// ---- values ----

type valuesOp struct {
	rows [][]expr.Expr
	pos  int
}

func (v *valuesOp) Next() (row, bool, error) {
	if v.pos >= len(v.rows) {
		return nil, false, nil
	}
	exprs := v.rows[v.pos]
	v.pos++
	out := make(row, len(exprs))
	for i, e := range exprs {
		val, err := e.Eval(nil)
		if err != nil {
			return nil, false, err
		}
		out[i] = val
	}
	return out, true, nil
}

func (v *valuesOp) Close() error { return nil }

// keyOf encodes a tuple for use as a map key. Hashing and grouping both need
// value equality, and the key encoding already defines it: two tuples encode
// identically exactly when their values compare equal.
func keyOf(vals []types.Value) string {
	return string(encoding.EncodeKeyValues(nil, vals...))
}
