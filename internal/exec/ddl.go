package exec

import (
	"fmt"
	"strings"

	"github.com/dhananjaypesu/quarry/internal/catalog"
	"github.com/dhananjaypesu/quarry/internal/encoding"
	"github.com/dhananjaypesu/quarry/internal/plan"
	"github.com/dhananjaypesu/quarry/internal/txn"
	"github.com/dhananjaypesu/quarry/internal/types"
)

func execCreateTable(tx *txn.Txn, cat *catalog.Catalog, p *plan.CreateTable) error {
	st := p.Stmt
	if _, exists := cat.Get(st.Name); exists {
		if st.IfNotExists {
			return nil
		}
		return fmt.Errorf("table %s already exists", st.Name)
	}

	t := &catalog.Table{ID: cat.AllocID(tx), Name: st.Name, NextRowID: 1}
	seen := map[string]bool{}
	for i, c := range st.Columns {
		if seen[strings.ToLower(c.Name)] {
			return fmt.Errorf("duplicate column %s", c.Name)
		}
		seen[strings.ToLower(c.Name)] = true
		t.Columns = append(t.Columns, catalog.Column{
			ID: uint32(i + 1), Name: c.Name, Type: c.Type, NotNull: c.NotNull,
		})
		if c.PrimaryKey {
			if len(t.PK) > 0 {
				return fmt.Errorf("table %s has more than one primary key", st.Name)
			}
			t.PK = []uint32{uint32(i + 1)}
		}
	}
	if len(st.PrimaryKey) > 0 {
		if len(t.PK) > 0 {
			return fmt.Errorf("table %s has more than one primary key", st.Name)
		}
		for _, name := range st.PrimaryKey {
			col, pos, ok := t.ColumnByName(name)
			if !ok {
				return fmt.Errorf("primary key column %q does not exist", name)
			}
			t.Columns[pos].NotNull = true
			t.PK = append(t.PK, col.ID)
		}
	}
	if len(t.PK) == 0 {
		// Every row needs a unique key to live at. A table that declares none
		// gets a hidden counter column, which is invisible to SELECT * but is
		// a perfectly ordinary column everywhere else in the engine.
		id := uint32(len(t.Columns) + 1)
		t.Columns = append(t.Columns, catalog.Column{
			ID: id, Name: catalog.RowIDColumn, Type: types.Int, NotNull: true, Hidden: true,
		})
		t.PK = []uint32{id}
	}
	return cat.Put(tx, t)
}

func execDropTable(tx *txn.Txn, cat *catalog.Catalog, p *plan.DropTable) error {
	t, ok := cat.Get(p.Stmt.Name)
	if !ok {
		if p.Stmt.IfExists {
			return nil
		}
		return fmt.Errorf("no such table: %s", p.Stmt.Name)
	}
	// Dropping the schema is not enough: the rows and index entries would
	// still be sitting in the store, and a later table could be given the same
	// id. Deleting the ranges keeps the keyspace honest.
	if err := deleteRange(tx, encoding.TableRowPrefix(t.ID)); err != nil {
		return err
	}
	for i := range t.Indexes {
		if err := deleteRange(tx, encoding.IndexPrefix(t.Indexes[i].ID)); err != nil {
			return err
		}
	}
	cat.Drop(tx, t.Name)
	return nil
}

func deleteRange(tx *txn.Txn, prefix []byte) error {
	it, err := tx.NewIter(prefix, encoding.PrefixEnd(prefix))
	if err != nil {
		return err
	}
	defer it.Close()
	var keys [][]byte
	for ; it.Valid(); it.Next() {
		keys = append(keys, append([]byte(nil), it.Key()...))
	}
	if err := it.Err(); err != nil {
		return err
	}
	for _, k := range keys {
		tx.Delete(k)
	}
	return nil
}

func execCreateIndex(tx *txn.Txn, cat *catalog.Catalog, p *plan.CreateIndex) (int64, error) {
	st := p.Stmt
	t := p.Table
	if _, exists := t.IndexByName(st.Name); exists {
		if st.IfNotExists {
			return 0, nil
		}
		return 0, fmt.Errorf("index %s already exists", st.Name)
	}
	idx := catalog.Index{ID: cat.AllocID(tx), Name: st.Name, Unique: st.Unique}
	for _, name := range st.Columns {
		col, _, ok := t.ColumnByName(name)
		if !ok {
			return 0, fmt.Errorf("no column %q in table %s", name, t.Name)
		}
		idx.Columns = append(idx.Columns, col.ID)
	}

	// Building the index means reading every existing row: an index created
	// after the data has to be populated from it, and doing that inside the
	// statement's transaction means a failure halfway through leaves neither a
	// half-built index nor a catalog entry pointing at one.
	it, err := tx.NewIter(encoding.TableRowPrefix(t.ID), encoding.PrefixEnd(encoding.TableRowPrefix(t.ID)))
	if err != nil {
		return 0, err
	}
	defer it.Close()

	scratch := *t
	scratch.Indexes = []catalog.Index{idx}
	var n int64
	for ; it.Valid(); it.Next() {
		r, err := decodeRow(t, it.Key(), it.Value())
		if err != nil {
			return n, err
		}
		if err := writeIndexes(tx, &scratch, r); err != nil {
			return n, err
		}
		n++
	}
	if err := it.Err(); err != nil {
		return n, err
	}
	t.Indexes = append(t.Indexes, idx)
	return n, cat.Put(tx, t)
}
