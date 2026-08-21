package exec

import (
	"github.com/dhananjaypesu/quarry/internal/expr"
	"github.com/dhananjaypesu/quarry/internal/plan"
	"github.com/dhananjaypesu/quarry/internal/sql"
	"github.com/dhananjaypesu/quarry/internal/txn"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// joinOp implements both join strategies.
//
// When the ON clause yields equality keys, the right input is drained into a
// hash table and the left input probes it: one pass over each side. Without
// keys there is nothing to hash on, so the right side is buffered and every
// left row is matched against all of it — quadratic, and named as such in
// EXPLAIN so that a missing ON clause is visible rather than merely slow.
//
// The right side is the build side in both cases. Without statistics there is
// no basis for choosing the smaller input, and buffering the right side keeps
// the left input streaming, which is what lets a LIMIT above the join stop
// early.
type joinOp struct {
	left, right Operator
	joinType    sql.JoinType
	leftKeys    []expr.Expr
	rightKeys   []expr.Expr
	extra       expr.Expr

	built   bool
	buckets map[string][]row
	all     []row
	width   int

	cur      row
	matches  []row
	matchPos int
	matched  bool
}

func newJoin(tx *txn.Txn, p *plan.Join) (Operator, error) {
	l, err := Build(tx, p.Left)
	if err != nil {
		return nil, err
	}
	r, err := Build(tx, p.Right)
	if err != nil {
		l.Close()
		return nil, err
	}
	return &joinOp{
		left: l, right: r, joinType: p.Type,
		leftKeys: p.LeftKeys, rightKeys: p.RightKeys, extra: p.Extra,
		width: len(p.Right.Columns()),
	}, nil
}

func (j *joinOp) build() error {
	j.built = true
	j.buckets = make(map[string][]row)
	for {
		r, ok, err := j.right.Next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		cp := append(row(nil), r...)
		if len(j.rightKeys) == 0 {
			j.all = append(j.all, cp)
			continue
		}
		key, null, err := evalKeys(j.rightKeys, cp)
		if err != nil {
			return err
		}
		if null {
			// A NULL key can never satisfy an equality, so the row simply does
			// not enter the hash table.
			continue
		}
		j.buckets[key] = append(j.buckets[key], cp)
	}
}

func (j *joinOp) Next() (row, bool, error) {
	if !j.built {
		if err := j.build(); err != nil {
			return nil, false, err
		}
	}
	for {
		if j.cur != nil {
			for j.matchPos < len(j.matches) {
				right := j.matches[j.matchPos]
				j.matchPos++
				out := append(append(row(nil), j.cur...), right...)
				ok, err := expr.Passes(j.extra, out)
				if err != nil {
					return nil, false, err
				}
				if !ok {
					continue
				}
				j.matched = true
				return out, true, nil
			}
			// A LEFT JOIN owes one NULL-extended row to every left row that
			// found no partner, which is the only reason this operator tracks
			// whether the current row matched.
			if !j.matched && j.joinType == sql.LeftJoin {
				j.matched = true
				out := append(append(row(nil), j.cur...), nullRow(j.width)...)
				j.cur = nil
				return out, true, nil
			}
			j.cur = nil
		}

		l, ok, err := j.left.Next()
		if err != nil || !ok {
			return nil, false, err
		}
		j.cur = append(row(nil), l...)
		j.matchPos, j.matched = 0, false
		if len(j.leftKeys) == 0 {
			j.matches = j.all
			continue
		}
		key, null, err := evalKeys(j.leftKeys, j.cur)
		if err != nil {
			return nil, false, err
		}
		if null {
			j.matches = nil
			continue
		}
		j.matches = j.buckets[key]
	}
}

func (j *joinOp) Close() error {
	err := j.left.Close()
	if rerr := j.right.Close(); err == nil {
		err = rerr
	}
	return err
}

func evalKeys(keys []expr.Expr, r row) (string, bool, error) {
	vals := make([]types.Value, len(keys))
	for i, k := range keys {
		v, err := k.Eval(r)
		if err != nil {
			return "", false, err
		}
		if v.Null {
			return "", true, nil
		}
		vals[i] = v
	}
	return keyOf(vals), false, nil
}

func nullRow(n int) row {
	r := make(row, n)
	for i := range r {
		r[i] = types.NullValue
	}
	return r
}
