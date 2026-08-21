// Package exec runs a plan.
//
// Every operator is a volcano-style iterator: Next pulls one tuple from its
// input, does its work, and returns one tuple. The whole tree therefore
// streams — a SELECT with a LIMIT stops the scan as soon as it has enough rows
// rather than materializing the table first. Only the operators that cannot
// stream by definition (sort, hash join build side, aggregation) buffer, and
// each says so where it does it.
package exec

import (
	"errors"
	"fmt"

	"github.com/dhananjaypesu/quarry/internal/catalog"
	"github.com/dhananjaypesu/quarry/internal/encoding"
	"github.com/dhananjaypesu/quarry/internal/txn"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// A row is one tuple. Operators are free to keep the slice they are handed
// only until the next call to Next, so anything that buffers must copy.
type row = []types.Value

// ErrDuplicateKey is returned when a write would put two rows at the same key.
var ErrDuplicateKey = errors.New("duplicate key")

// rowKey builds the storage key for a row: the table prefix followed by the
// primary key columns in declaration order. Because the encoding is order
// preserving, rows land in primary-key order, and a range of primary keys is a
// range of storage keys.
func rowKey(t *catalog.Table, r row) []byte {
	key := encoding.TableRowPrefix(t.ID)
	for _, id := range t.PK {
		key = encoding.EncodeKeyValue(key, r[t.Pos(id)])
	}
	return key
}

// rowValue encodes the columns that are not part of the primary key. The key
// columns are omitted because they are already in the key; storing them twice
// would inflate every row for no gain.
func rowValue(t *catalog.Table, r row) []byte {
	ids := make([]uint32, 0, len(t.Columns))
	vals := make([]types.Value, 0, len(t.Columns))
	for i, c := range t.Columns {
		if isPKColumn(t, c.ID) {
			continue
		}
		ids = append(ids, c.ID)
		vals = append(vals, r[i])
	}
	return encoding.EncodeRow(nil, ids, vals)
}

func isPKColumn(t *catalog.Table, id uint32) bool {
	for _, pk := range t.PK {
		if pk == id {
			return true
		}
	}
	return false
}

// newRow returns a tuple of typed NULLs, which is the correct starting point
// for decoding: a column absent from the encoding is absent because it is
// NULL.
func newRow(t *catalog.Table) row {
	r := make(row, len(t.Columns))
	for i, c := range t.Columns {
		r[i] = types.NullOf(c.Type)
	}
	return r
}

// decodeRow reconstructs a tuple from a stored key and value.
func decodeRow(t *catalog.Table, key, value []byte) (row, error) {
	r := newRow(t)
	rest := key[len(encoding.TableRowPrefix(t.ID)):]
	for _, id := range t.PK {
		v, tail, err := encoding.DecodeKeyValue(rest)
		if err != nil {
			return nil, err
		}
		r[t.Pos(id)] = v
		rest = tail
	}
	if err := encoding.DecodeRow(value, t.PosMap(), r); err != nil {
		return nil, err
	}
	return r, nil
}

// indexEntry builds the key and value of one index entry for a row.
//
// The primary key is stored in the entry's value, so an index scan can find
// the row without parsing the key. For a unique index it is also left out of
// the key, which is what makes the uniqueness check a single point read: two
// rows with the same indexed values would collide on the same key. That trick
// is only sound when no indexed column is NULL, because SQL holds that NULLs
// do not equal each other and so must not collide.
func indexEntry(t *catalog.Table, idx *catalog.Index, r row) (key, value []byte) {
	key = encoding.IndexPrefix(idx.ID)
	hasNull := false
	for _, id := range idx.Columns {
		v := r[t.Pos(id)]
		if v.Null {
			hasNull = true
		}
		key = encoding.EncodeKeyValue(key, v)
	}
	var pk []byte
	for _, id := range t.PK {
		pk = encoding.EncodeKeyValue(pk, r[t.Pos(id)])
	}
	if !idx.Unique || hasNull {
		key = append(key, pk...)
	}
	return key, pk
}

// indexIsEnforceable reports whether an entry participates in the uniqueness
// check, which is exactly the case where its key omits the primary key.
func indexIsEnforceable(t *catalog.Table, idx *catalog.Index, r row) bool {
	if !idx.Unique {
		return false
	}
	for _, id := range idx.Columns {
		if r[t.Pos(id)].Null {
			return false
		}
	}
	return true
}

// insertRow writes a row and every index entry that points at it, refusing to
// overwrite an existing primary key or to break a unique index.
func insertRow(tx *txn.Txn, t *catalog.Table, r row) error {
	for i, c := range t.Columns {
		if c.NotNull && r[i].Null {
			return fmt.Errorf("column %s cannot be NULL", c.Name)
		}
	}
	key := rowKey(t, r)
	if _, err := tx.Get(key); err == nil {
		return fmt.Errorf("%w: %s already has a row with this primary key", ErrDuplicateKey, t.Name)
	} else if !errors.Is(err, txn.ErrNotFound) {
		return err
	}
	if err := writeIndexes(tx, t, r); err != nil {
		return err
	}
	tx.Set(key, rowValue(t, r))
	return nil
}

func writeIndexes(tx *txn.Txn, t *catalog.Table, r row) error {
	for i := range t.Indexes {
		idx := &t.Indexes[i]
		key, val := indexEntry(t, idx, r)
		if indexIsEnforceable(t, idx, r) {
			if _, err := tx.Get(key); err == nil {
				return fmt.Errorf("%w: index %s is unique", ErrDuplicateKey, idx.Name)
			} else if !errors.Is(err, txn.ErrNotFound) {
				return err
			}
		}
		tx.Set(key, val)
	}
	return nil
}

// deleteRow removes a row and its index entries.
func deleteRow(tx *txn.Txn, t *catalog.Table, r row) {
	for i := range t.Indexes {
		key, _ := indexEntry(t, &t.Indexes[i], r)
		tx.Delete(key)
	}
	tx.Delete(rowKey(t, r))
}
