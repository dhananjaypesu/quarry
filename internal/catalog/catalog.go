// Package catalog stores and serves the schema.
//
// The catalog lives in the same key-value store as the data, under its own key
// prefix. That is not just tidiness: because a schema change is an ordinary
// write, a CREATE TABLE and the rows that depend on it commit through the same
// batch, and a crash can never leave the store holding rows for a table that
// does not exist.
package catalog

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/dhananjaypesu/quarry/internal/encoding"
	"github.com/dhananjaypesu/quarry/internal/txn"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// RowIDColumn is the name of the hidden column that stands in for a primary
// key when a table declares none. Every table needs a unique row key, and
// inventing one is cheaper than refusing the table.
const RowIDColumn = "_rowid"

// Column is one column of a table.
type Column struct {
	ID      uint32     `json:"id"`
	Name    string     `json:"name"`
	Type    types.Type `json:"type"`
	NotNull bool       `json:"not_null"`
	Hidden  bool       `json:"hidden,omitempty"`
}

// Index is a secondary index. Its entries are keys of the form
// (index id, indexed values, primary key) with an empty value: everything the
// index needs to say is expressible in the sort order, so storing a payload
// would only add write amplification.
type Index struct {
	ID      uint32   `json:"id"`
	Name    string   `json:"name"`
	Columns []uint32 `json:"columns"`
	Unique  bool     `json:"unique"`
}

// Table is a table's schema.
type Table struct {
	ID        uint32   `json:"id"`
	Name      string   `json:"name"`
	Columns   []Column `json:"columns"`
	PK        []uint32 `json:"pk"`
	Indexes   []Index  `json:"indexes"`
	NextRowID int64    `json:"next_rowid"`

	// pos maps a column id to its index in Columns, which is also its position
	// in every tuple the engine builds for this table.
	pos map[uint32]int
}

// ColumnByName looks a column up case-insensitively, as SQL requires.
func (t *Table) ColumnByName(name string) (*Column, int, bool) {
	for i := range t.Columns {
		if strings.EqualFold(t.Columns[i].Name, name) {
			return &t.Columns[i], i, true
		}
	}
	return nil, 0, false
}

// Pos returns the tuple position of a column id.
func (t *Table) Pos(id uint32) int {
	if t.pos == nil {
		t.pos = make(map[uint32]int, len(t.Columns))
		for i, c := range t.Columns {
			t.pos[c.ID] = i
		}
	}
	return t.pos[id]
}

// PosMap returns the id-to-position map the row decoder needs.
func (t *Table) PosMap() map[uint32]int {
	t.Pos(0) // force the map to be built
	return t.pos
}

// Visible returns the columns a "SELECT *" should expand to, which excludes
// the synthetic row id.
func (t *Table) Visible() []Column {
	out := make([]Column, 0, len(t.Columns))
	for _, c := range t.Columns {
		if !c.Hidden {
			out = append(out, c)
		}
	}
	return out
}

// HasRowID reports whether the table's key is the synthetic row id.
func (t *Table) HasRowID() bool {
	return len(t.PK) == 1 && t.Columns[t.Pos(t.PK[0])].Hidden
}

// IndexByName finds an index on this table.
func (t *Table) IndexByName(name string) (*Index, bool) {
	for i := range t.Indexes {
		if strings.EqualFold(t.Indexes[i].Name, name) {
			return &t.Indexes[i], true
		}
	}
	return nil, false
}

// Catalog caches every schema in memory. Schemas are read on the path of every
// statement and change rarely, so the cache is refreshed on write rather than
// consulted through the store each time.
type Catalog struct {
	mu     sync.RWMutex
	tables map[string]*Table // keyed by lower-cased name
	nextID uint32
}

const nextIDKey = "nextid"

// Load reads the whole catalog. It runs once at open: the number of tables is
// small, and holding them all avoids a store read per statement.
func Load(t *txn.Txn) (*Catalog, error) {
	c := &Catalog{tables: make(map[string]*Table), nextID: 1}
	prefix := encoding.MetaKey("table")
	it, err := t.NewIter(prefix, encoding.PrefixEnd(prefix))
	if err != nil {
		return nil, err
	}
	defer it.Close()
	for ; it.Valid(); it.Next() {
		var tbl Table
		if err := json.Unmarshal(it.Value(), &tbl); err != nil {
			return nil, fmt.Errorf("quarry: corrupt catalog entry: %w", err)
		}
		c.tables[strings.ToLower(tbl.Name)] = &tbl
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	if v, err := t.Get(encoding.MetaKey(nextIDKey)); err == nil {
		var n uint32
		if _, err := fmt.Sscanf(string(v), "%d", &n); err == nil {
			c.nextID = n
		}
	}
	return c, nil
}

// Get returns a table by name.
func (c *Catalog) Get(name string) (*Table, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tables[strings.ToLower(name)]
	return t, ok
}

// Names lists every table in sorted order.
func (c *Catalog) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.tables))
	for _, t := range c.tables {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// AllocID hands out the next object id. Table and index ids share one sequence
// so that no id is ever reused across object kinds, which keeps a stale key
// from being mistaken for a live one after a drop.
func (c *Catalog) AllocID(t *txn.Txn) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.nextID++
	t.Set(encoding.MetaKey(nextIDKey), []byte(fmt.Sprintf("%d", c.nextID)))
	return id
}

// Put writes a schema through the transaction and updates the cache. The cache
// is updated immediately rather than on commit because the statement that
// created the table may go on to insert into it; a rollback reloads the
// catalog from the store, which is the cost of keeping the fast path simple.
func (c *Catalog) Put(t *txn.Txn, tbl *Table) error {
	b, err := json.Marshal(tbl)
	if err != nil {
		return err
	}
	t.Set(encoding.MetaKey("table", strings.ToLower(tbl.Name)), b)
	c.mu.Lock()
	c.tables[strings.ToLower(tbl.Name)] = tbl
	c.mu.Unlock()
	return nil
}

// Drop removes a schema.
func (c *Catalog) Drop(t *txn.Txn, name string) {
	t.Delete(encoding.MetaKey("table", strings.ToLower(name)))
	c.mu.Lock()
	delete(c.tables, strings.ToLower(name))
	c.mu.Unlock()
}

// Reload discards the cache and reads it again, which is how a rolled-back DDL
// statement is undone.
func (c *Catalog) Reload(t *txn.Txn) error {
	fresh, err := Load(t)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.tables, c.nextID = fresh.tables, fresh.nextID
	c.mu.Unlock()
	return nil
}
