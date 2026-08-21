package plan

import (
	"fmt"
	"strings"

	"github.com/dhananjaypesu/quarry/internal/catalog"
	"github.com/dhananjaypesu/quarry/internal/encoding"
	"github.com/dhananjaypesu/quarry/internal/expr"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// Access-path selection is the one place this engine optimizes for real.
//
// Every table lives in one ordered keyspace, so a predicate is useful to the
// planner only when it can be turned into a contiguous byte range. The
// procedure is the classic one: split the predicate into conjuncts, keep those
// of the form `column op constant`, and then, for each candidate path, match
// its key columns left to right — equalities extend the prefix, and the first
// column without an equality may contribute a range. A path that matches
// nothing degenerates to a full scan, which is the correct answer often
// enough that it is the default rather than a failure.

// constraint is a `column op constant` conjunct, with the column named by its
// position in the scanned table's tuple.
type constraint struct {
	pos int
	op  string
	val types.Value
	src expr.Expr
}

// splitConjuncts flattens an AND tree. OR is left intact: a disjunction can
// only be used for a key range by taking the union of its branches, which this
// engine does not do.
func splitConjuncts(e expr.Expr) []expr.Expr {
	if e == nil {
		return nil
	}
	if b, ok := e.(*expr.Binary); ok && b.Op == "AND" {
		return append(splitConjuncts(b.L), splitConjuncts(b.R)...)
	}
	return []expr.Expr{e}
}

// combine rebuilds a conjunction, returning nil for the empty set.
func combine(parts []expr.Expr) expr.Expr {
	var out expr.Expr
	for _, p := range parts {
		if out == nil {
			out = p
			continue
		}
		out = &expr.Binary{Op: "AND", L: out, R: p, Typ: types.Bool}
	}
	return out
}

func analyze(conjuncts []expr.Expr) []constraint {
	var out []constraint
	for _, c := range conjuncts {
		b, ok := c.(*expr.Binary)
		if !ok {
			continue
		}
		var col *expr.Column
		var lit *expr.Const
		op := b.Op
		if l, ok := b.L.(*expr.Column); ok {
			if r, ok := b.R.(*expr.Const); ok {
				col, lit = l, r
			}
		}
		if col == nil {
			if r, ok := b.R.(*expr.Column); ok {
				if l, ok := b.L.(*expr.Const); ok {
					col, lit = r, l
					op = flip(op)
				}
			}
		}
		if col == nil || lit == nil || lit.Val.Null {
			// `col = NULL` is never true, and turning it into a key range
			// would wrongly match rows whose key encodes as NULL.
			continue
		}
		switch op {
		case "=", "<", "<=", ">", ">=":
			out = append(out, constraint{pos: col.Pos, op: op, val: lit.Val, src: c})
		}
	}
	return out
}

func flip(op string) string {
	switch op {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	}
	return op
}

// path is a candidate access path under consideration.
type path struct {
	index    *catalog.Index // nil for the primary key
	start    []byte
	end      []byte
	access   string
	consumed map[expr.Expr]bool
	score    int
}

// choosePath picks an access path for a scan of t given the conjuncts that
// apply to it. The returned Scan carries the key range and the predicate still
// to be evaluated on each row the range produces.
func choosePath(t *catalog.Table, conjuncts []expr.Expr) *Scan {
	cons := analyze(conjuncts)

	best := buildPath(t, nil, t.PK, cons)
	for i := range t.Indexes {
		idx := &t.Indexes[i]
		// An index has to beat the primary key outright: reaching a row
		// through an index costs a second lookup, so a tie is not a win.
		if p := buildPath(t, idx, idx.Columns, cons); p.score > best.score {
			best = p
		}
	}

	var residual []expr.Expr
	for _, c := range conjuncts {
		if !best.consumed[c] {
			residual = append(residual, c)
		}
	}
	return &Scan{
		Table:  t,
		Index:  best.index,
		Start:  best.start,
		End:    best.end,
		Access: best.access,
		Filter: combine(residual),
	}
}

func buildPath(t *catalog.Table, idx *catalog.Index, keyCols []uint32, cons []constraint) *path {
	var prefix []byte
	if idx == nil {
		prefix = encoding.TableRowPrefix(t.ID)
	} else {
		prefix = encoding.IndexPrefix(idx.ID)
	}
	p := &path{index: idx, consumed: make(map[expr.Expr]bool)}

	key := append([]byte(nil), prefix...)
	var matched []string
	nEq := 0
	for _, colID := range keyCols {
		pos := t.Pos(colID)
		c, ok := findConstraint(cons, pos, "=")
		if !ok {
			break
		}
		v, ok := coerce(c.val, t.Columns[pos].Type)
		if !ok {
			break
		}
		key = encoding.EncodeKeyValue(key, v)
		p.consumed[c.src] = true
		matched = append(matched, fmt.Sprintf("%s = %s", t.Columns[pos].Name, c.val))
		nEq++
	}
	p.score = nEq * 2
	p.start = key
	p.end = encoding.PrefixEnd(key)

	// The first unmatched key column can still contribute a range. Range
	// bounds are not marked consumed: the encoded bound is inclusive at byte
	// granularity, and re-checking the conjunct per row is cheaper than
	// reasoning about the boundary key.
	if nEq < len(keyCols) {
		pos := t.Pos(keyCols[nEq])
		lo, hasLo := rangeBound(cons, pos, ">", ">=")
		hi, hasHi := rangeBound(cons, pos, "<", "<=")
		if hasLo {
			if v, ok := coerce(lo.val, t.Columns[pos].Type); ok {
				b := encoding.EncodeKeyValue(append([]byte(nil), key...), v)
				if lo.op == ">" {
					b = encoding.PrefixEnd(b)
				}
				if b != nil {
					p.start = b
					p.score++
					matched = append(matched, fmt.Sprintf("%s %s %s", t.Columns[pos].Name, lo.op, lo.val))
				}
			}
		}
		if hasHi {
			if v, ok := coerce(hi.val, t.Columns[pos].Type); ok {
				b := encoding.EncodeKeyValue(append([]byte(nil), key...), v)
				if hi.op == "<=" {
					b = encoding.PrefixEnd(b)
				}
				if b != nil {
					p.end = b
					p.score++
					matched = append(matched, fmt.Sprintf("%s %s %s", t.Columns[pos].Name, hi.op, hi.val))
				}
			}
		}
	}

	p.access = describe(t, idx, keyCols, nEq, matched)
	return p
}

func describe(t *catalog.Table, idx *catalog.Index, keyCols []uint32, nEq int, matched []string) string {
	name := "primary key"
	if idx != nil {
		name = "index " + idx.Name
	}
	if len(matched) == 0 {
		if idx == nil {
			return "full scan"
		}
		return "full scan of " + name
	}
	kind := "range"
	if nEq == len(keyCols) && nEq == len(matched) {
		// Every key column is pinned, so at most one row (or, for a
		// non-unique index, one key group) can match.
		if idx == nil || idx.Unique {
			kind = "lookup"
		}
	}
	return fmt.Sprintf("%s %s: %s", name, kind, strings.Join(matched, ", "))
}

func findConstraint(cons []constraint, pos int, op string) (constraint, bool) {
	for _, c := range cons {
		if c.pos == pos && c.op == op {
			return c, true
		}
	}
	return constraint{}, false
}

// rangeBound picks the tighter of the two spellings of a bound if both are
// present, preferring the strict one because it excludes more.
func rangeBound(cons []constraint, pos int, strict, loose string) (constraint, bool) {
	var out constraint
	found := false
	for _, c := range cons {
		if c.pos != pos || (c.op != strict && c.op != loose) {
			continue
		}
		if !found {
			out, found = c, true
			continue
		}
		cmp, ok := types.Compare(c.val, out.val)
		if !ok {
			continue
		}
		tighter := cmp > 0
		if strict == "<" {
			tighter = cmp < 0
		}
		if tighter || (cmp == 0 && c.op == strict) {
			out = c
		}
	}
	return out, found
}

// coerce prepares a constant for key encoding. The encoding is only order
// preserving between values of the same type, so a constant that cannot be
// represented in the column's type must not become a bound.
func coerce(v types.Value, t types.Type) (types.Value, bool) {
	if v.Type == t {
		return v, true
	}
	if v.Type == types.Int && t == types.Float {
		return types.NewFloat(float64(v.I)), true
	}
	return types.Value{}, false
}
