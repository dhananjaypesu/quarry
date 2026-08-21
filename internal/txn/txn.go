// Package txn gives the query engine transactional reads and writes over the
// key-value store.
//
// The storage engine already provides the two primitives a transaction needs:
// a snapshot pins a sequence number so reads see a consistent view no matter
// what commits underneath, and a batch applies a set of mutations atomically.
// What it does not provide is *read-your-own-writes*: a statement inside a
// transaction has to see rows that earlier statements in the same transaction
// wrote but that are not committed yet. That is the job of the write set kept
// here, and of the overlay iterator that merges it with the snapshot.
package txn

import (
	"bytes"
	"errors"
	"sort"

	"github.com/dhananjaypesu/lsmdb"
)

// ErrNotFound is returned by Get when a key has no visible value.
var ErrNotFound = errors.New("quarry: key not found")

// ErrDone is returned when a transaction is used after commit or rollback.
var ErrDone = errors.New("quarry: transaction is already finished")

// Txn is a read-write transaction. It is not safe for concurrent use by
// multiple goroutines, matching the one-transaction-per-session model of the
// shell and of a connection in a real server.
type Txn struct {
	db   *lsmdb.DB
	snap *lsmdb.Snapshot

	// writes holds the uncommitted mutations, with a nil value meaning a
	// delete. Keys are held as strings so they can be map keys; the extra copy
	// is what makes the overlay safe against callers reusing key buffers.
	writes map[string][]byte
	// sorted is the write set in key order, rebuilt only when an iterator is
	// opened after a mutation. Statements typically write many rows and then
	// scan once, so rebuilding beats maintaining order on every Set.
	sorted []string
	dirty  bool

	done bool
}

// Begin starts a transaction against the current committed state.
func Begin(db *lsmdb.DB) *Txn {
	return &Txn{db: db, snap: db.NewSnapshot(), writes: make(map[string][]byte)}
}

// Get returns the value for key, honoring uncommitted writes first.
func (t *Txn) Get(key []byte) ([]byte, error) {
	if t.done {
		return nil, ErrDone
	}
	if v, ok := t.writes[string(key)]; ok {
		if v == nil {
			return nil, ErrNotFound
		}
		return v, nil
	}
	v, err := t.snap.Get(key)
	if errors.Is(err, lsmdb.ErrNotFound) {
		return nil, ErrNotFound
	}
	return v, err
}

// Set stages a write.
func (t *Txn) Set(key, value []byte) {
	cp := make([]byte, len(value))
	copy(cp, value)
	t.writes[string(key)] = cp
	t.dirty = true
}

// Delete stages a deletion.
func (t *Txn) Delete(key []byte) {
	t.writes[string(key)] = nil
	t.dirty = true
}

// Commit applies every staged mutation as one atomic batch. An empty write set
// commits without touching the log, which keeps read-only transactions free.
func (t *Txn) Commit() error {
	if t.done {
		return ErrDone
	}
	t.done = true
	defer t.snap.Close()
	if len(t.writes) == 0 {
		return nil
	}
	b := lsmdb.NewBatch()
	for _, k := range t.sortedKeys() {
		if v := t.writes[k]; v == nil {
			b.Delete([]byte(k))
		} else {
			b.Set([]byte(k), v)
		}
	}
	return t.db.Write(b)
}

// Rollback discards the write set. Because nothing was written to the store,
// undoing a transaction costs nothing — the price is paid on commit instead.
func (t *Txn) Rollback() {
	if t.done {
		return
	}
	t.done = true
	t.writes = nil
	t.snap.Close()
}

// Done reports whether the transaction has been committed or rolled back.
func (t *Txn) Done() bool { return t.done }

func (t *Txn) sortedKeys() []string {
	if t.dirty {
		// A fresh slice, never a reused one: an open iterator holds the slice
		// it was given, and rewriting that backing array underneath it while
		// a statement writes during a scan would corrupt the iteration.
		t.sorted = make([]string, 0, len(t.writes))
		for k := range t.writes {
			t.sorted = append(t.sorted, k)
		}
		sort.Strings(t.sorted)
		t.dirty = false
	}
	return t.sorted
}

// Iter is a forward iterator over [start, end) that merges the transaction's
// uncommitted writes with the snapshot.
//
// The merge is the same two-way merge an LSM tree does internally, with one
// rule: on equal keys the write set wins, because it is newer. A staged delete
// therefore hides a committed row without any tombstone reaching disk.
type Iter struct {
	txn       *Txn
	end       []byte
	base      *lsmdb.Iterator
	baseValid bool

	over    []string
	overPos int

	key   []byte
	value []byte
	valid bool
	err   error
}

// NewIter opens an iterator over the half-open range [start, end). A nil end
// means "to the end of the keyspace".
func (t *Txn) NewIter(start, end []byte) (*Iter, error) {
	if t.done {
		return nil, ErrDone
	}
	base, err := t.snap.NewIter()
	if err != nil {
		return nil, err
	}
	it := &Iter{txn: t, end: end, base: base}
	it.baseValid = base.Seek(start)
	keys := t.sortedKeys()
	it.over = keys
	it.overPos = sort.SearchStrings(keys, string(start))
	it.advance()
	return it, nil
}

func (it *Iter) inRange(k []byte) bool {
	return it.end == nil || bytes.Compare(k, it.end) < 0
}

// advance positions the iterator on the next visible key, skipping over
// staged deletions.
func (it *Iter) advance() {
	for {
		var baseKey []byte
		if it.baseValid && it.inRange(it.base.Key()) {
			baseKey = it.base.Key()
		} else {
			// Either the base iterator is exhausted or it has walked past the
			// end of the range; both mean it has nothing more to contribute.
			it.baseValid = false
			baseKey = nil
		}
		var overKey []byte
		if it.overPos < len(it.over) && it.inRange([]byte(it.over[it.overPos])) {
			overKey = []byte(it.over[it.overPos])
		}

		switch {
		case baseKey == nil && overKey == nil:
			it.valid = false
			if err := it.base.Err(); err != nil {
				it.err = err
			}
			return
		case overKey == nil:
			it.emit(baseKey, it.base.Value())
			it.baseValid = it.base.Next()
			return
		case baseKey == nil:
			v := it.txn.writes[it.over[it.overPos]]
			it.overPos++
			if v == nil {
				continue // staged delete of a key the snapshot never had
			}
			it.emit(overKey, v)
			return
		}

		cmp := bytes.Compare(baseKey, overKey)
		switch {
		case cmp < 0:
			it.emit(baseKey, it.base.Value())
			it.baseValid = it.base.Next()
			return
		case cmp > 0:
			v := it.txn.writes[it.over[it.overPos]]
			it.overPos++
			if v == nil {
				continue
			}
			it.emit(overKey, v)
			return
		default:
			// Same key on both sides: the uncommitted version shadows the
			// committed one, whether it is a value or a deletion.
			v := it.txn.writes[it.over[it.overPos]]
			it.overPos++
			it.baseValid = it.base.Next()
			if v == nil {
				continue
			}
			it.emit(overKey, v)
			return
		}
	}
}

func (it *Iter) emit(k, v []byte) {
	it.key = append(it.key[:0], k...)
	it.value = append(it.value[:0], v...)
	it.valid = true
}

// Valid reports whether the iterator is positioned on a row.
func (it *Iter) Valid() bool { return it.valid && it.err == nil }

// Next advances to the following row.
func (it *Iter) Next() { it.advance() }

// Key returns the current key. The slice is owned by the iterator and is
// invalidated by Next, so callers that keep it must copy.
func (it *Iter) Key() []byte { return it.key }

// Value returns the current value, with the same lifetime rule as Key.
func (it *Iter) Value() []byte { return it.value }

// Err reports an error encountered during iteration.
func (it *Iter) Err() error { return it.err }

// Close releases the underlying storage iterator, which is holding table
// references that compaction cannot reclaim until it is released.
func (it *Iter) Close() error { return it.base.Close() }
