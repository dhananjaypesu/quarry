package exec

import (
	"fmt"

	"github.com/dhananjaypesu/quarry/internal/catalog"
	"github.com/dhananjaypesu/quarry/internal/plan"
	"github.com/dhananjaypesu/quarry/internal/txn"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// Execute runs a statement that changes data or schema and reports how many
// rows it touched.
//
// Mutations do not stream. Each one drains its input into memory before
// writing anything, because the input reads through the same transaction the
// writes go into: an UPDATE that wrote as it scanned would see its own new
// rows through the overlay iterator and, for a statement that moves a row
// forward in key order, would meet the row again and rewrite it forever.
// Reading first and writing second is what makes a statement see the table as
// it was when the statement began, which is what SQL requires anyway.
func Execute(tx *txn.Txn, cat *catalog.Catalog, n plan.Node) (int64, error) {
	switch p := n.(type) {
	case *plan.Insert:
		return execInsert(tx, cat, p)
	case *plan.Update:
		return execUpdate(tx, p)
	case *plan.Delete:
		return execDelete(tx, p)
	case *plan.CreateTable:
		return 0, execCreateTable(tx, cat, p)
	case *plan.DropTable:
		return 0, execDropTable(tx, cat, p)
	case *plan.CreateIndex:
		return execCreateIndex(tx, cat, p)
	}
	return 0, fmt.Errorf("cannot execute statement node %T", n)
}

func execInsert(tx *txn.Txn, cat *catalog.Catalog, p *plan.Insert) (int64, error) {
	in, err := Build(tx, p.Input)
	if err != nil {
		return 0, err
	}
	defer in.Close()

	t := p.Table
	var n int64
	for {
		src, ok, err := in.Next()
		if err != nil {
			return n, err
		}
		if !ok {
			break
		}
		r := newRow(t)
		for i, pos := range p.Positions {
			v, err := types.Cast(src[i], t.Columns[pos].Type)
			if err != nil {
				return n, fmt.Errorf("column %s: %w", t.Columns[pos].Name, err)
			}
			r[pos] = v
		}
		if t.HasRowID() {
			// A table without a declared key gets a synthetic one. The counter
			// lives in the catalog and is written through the same transaction
			// as the rows, so a rolled-back INSERT does not burn row ids.
			r[t.Pos(t.PK[0])] = types.NewInt(t.NextRowID)
			t.NextRowID++
			if err := cat.Put(tx, t); err != nil {
				return n, err
			}
		}
		if err := insertRow(tx, t, r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func execUpdate(tx *txn.Txn, p *plan.Update) (int64, error) {
	rows, err := collect(tx, p.Input)
	if err != nil {
		return 0, err
	}
	t := p.Table
	var n int64
	for _, old := range rows {
		next := append(row(nil), old...)
		for _, set := range p.Set {
			v, err := set.Expr.Eval(old)
			if err != nil {
				return n, err
			}
			cast, err := types.Cast(v, t.Columns[set.Pos].Type)
			if err != nil {
				return n, fmt.Errorf("column %s: %w", t.Columns[set.Pos].Name, err)
			}
			next[set.Pos] = cast
		}
		for i, c := range t.Columns {
			if c.NotNull && next[i].Null {
				return n, fmt.Errorf("column %s cannot be NULL", c.Name)
			}
		}
		// Changing a primary key moves the row to a different storage key, so
		// it is a delete plus an insert rather than an overwrite. Doing it in
		// that order also gets the uniqueness check for free.
		oldKey, newKey := rowKey(t, old), rowKey(t, next)
		deleteRow(tx, t, old)
		if string(oldKey) != string(newKey) {
			if err := insertRow(tx, t, next); err != nil {
				return n, err
			}
		} else {
			if err := writeIndexes(tx, t, next); err != nil {
				return n, err
			}
			tx.Set(newKey, rowValue(t, next))
		}
		n++
	}
	return n, nil
}

func execDelete(tx *txn.Txn, p *plan.Delete) (int64, error) {
	rows, err := collect(tx, p.Input)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		deleteRow(tx, p.Table, r)
	}
	return int64(len(rows)), nil
}

func collect(tx *txn.Txn, n plan.Node) ([]row, error) {
	op, err := Build(tx, n)
	if err != nil {
		return nil, err
	}
	defer op.Close()
	var out []row
	for {
		r, ok, err := op.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, append(row(nil), r...))
	}
}
