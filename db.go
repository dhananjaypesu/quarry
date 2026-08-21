// Package quarry is a SQL database built on the lsmdb storage engine.
//
// The layering is the point of the project. lsmdb provides an ordered,
// crash-safe, transactional key-value store and nothing else — no rows, no
// types, no query language. Everything above that line lives here: an
// encoding that makes SQL values sort correctly as bytes, a catalog that
// describes tables in terms of those keys, a parser and binder, an
// access-path chooser that turns predicates into key ranges, and a volcano
// executor. A statement therefore travels a straight path:
//
//	SQL text -> tokens -> AST -> bound plan -> key ranges -> operators -> rows
//
// A session holds at most one explicit transaction. Statements outside one run
// in an implicit transaction that commits if the statement succeeds and rolls
// back if it does not, which is the same contract as SQLite's autocommit mode.
package quarry

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dhananjaypesu/lsmdb"
	"github.com/dhananjaypesu/quarry/internal/catalog"
	"github.com/dhananjaypesu/quarry/internal/exec"
	"github.com/dhananjaypesu/quarry/internal/plan"
	"github.com/dhananjaypesu/quarry/internal/sql"
	"github.com/dhananjaypesu/quarry/internal/txn"
	"github.com/dhananjaypesu/quarry/internal/types"
)

// Value is a SQL datum in a result row.
type Value = types.Value

// DB is a database handle. It is safe for concurrent use only in the sense the
// storage engine is: statements serialize on the write path, and a session
// should not be shared across goroutines.
type DB struct {
	store *lsmdb.DB
	cat   *catalog.Catalog

	// tx is the session's explicit transaction, non-nil between BEGIN and
	// COMMIT or ROLLBACK.
	tx *txn.Txn
}

// Options configures the underlying storage engine.
type Options = lsmdb.Options

// DefaultOptions returns the storage engine's defaults.
func DefaultOptions() Options { return lsmdb.DefaultOptions() }

// Open opens or creates a database in dir.
func Open(dir string, opts Options) (*DB, error) {
	store, err := lsmdb.Open(dir, opts)
	if err != nil {
		return nil, err
	}
	boot := txn.Begin(store)
	cat, err := catalog.Load(boot)
	boot.Rollback()
	if err != nil {
		store.Close()
		return nil, err
	}
	return &DB{store: store, cat: cat}, nil
}

// Close flushes and closes the database, rolling back any open transaction.
func (db *DB) Close() error {
	if db.tx != nil {
		db.tx.Rollback()
		db.tx = nil
	}
	return db.store.Close()
}

// Result is the outcome of one statement: either a row set, or a count of
// rows affected, or the text of a plan.
type Result struct {
	Columns  []string
	Rows     [][]Value
	Rows_    int64 // deprecated placeholder to keep the struct extensible
	Affected int64
	Text     string
	// Statement echoes the kind of statement that produced the result, which
	// is what lets a shell decide between printing a table and printing a
	// count.
	Kind Kind
}

// Kind distinguishes what a Result carries.
type Kind uint8

const (
	KindRows Kind = iota
	KindAffected
	KindText
	KindNone
)

// Exec runs one or more statements separated by semicolons and returns the
// result of each.
func (db *DB) Exec(script string) ([]Result, error) {
	stmts, err := sql.ParseMany(script)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, st := range stmts {
		res, err := db.execOne(st)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// Query runs a single statement that is expected to return rows.
func (db *DB) Query(stmt string) (*Result, error) {
	st, err := sql.Parse(stmt)
	if err != nil {
		return nil, err
	}
	res, err := db.execOne(st)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

func (db *DB) execOne(st sql.Statement) (Result, error) {
	switch s := st.(type) {
	case *sql.Begin:
		if db.tx != nil {
			return Result{}, errors.New("a transaction is already open")
		}
		db.tx = txn.Begin(db.store)
		return Result{Kind: KindNone}, nil
	case *sql.Commit:
		if db.tx == nil {
			return Result{}, errors.New("no transaction is open")
		}
		err := db.tx.Commit()
		db.tx = nil
		return Result{Kind: KindNone}, err
	case *sql.Rollback:
		if db.tx == nil {
			return Result{}, errors.New("no transaction is open")
		}
		db.tx.Rollback()
		db.tx = nil
		// The catalog cache may hold schemas the rolled-back transaction
		// created, so it has to be rebuilt from what actually committed.
		reload := txn.Begin(db.store)
		err := db.cat.Reload(reload)
		reload.Rollback()
		return Result{Kind: KindNone}, err
	case *sql.ShowTables:
		names := db.cat.Names()
		rows := make([][]Value, len(names))
		for i, n := range names {
			rows[i] = []Value{types.NewText(n)}
		}
		return Result{Kind: KindRows, Columns: []string{"table"}, Rows: rows}, nil
	case *sql.Describe:
		return db.describe(s.Table)
	case *sql.Explain:
		return db.explain(s.Stmt)
	}
	return db.run(st)
}

// run executes a statement inside the session transaction, or inside an
// implicit one that lives exactly as long as the statement.
func (db *DB) run(st sql.Statement) (Result, error) {
	tx, implicit := db.tx, false
	if tx == nil {
		tx, implicit = txn.Begin(db.store), true
	}

	res, err := db.runIn(tx, st)
	if !implicit {
		if err != nil {
			return Result{}, err
		}
		return res, nil
	}
	if err != nil {
		tx.Rollback()
		// A failed DDL statement may have already updated the in-memory
		// catalog, which the rollback did not undo.
		reload := txn.Begin(db.store)
		db.cat.Reload(reload)
		reload.Rollback()
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, err
	}
	return res, nil
}

func (db *DB) runIn(tx *txn.Txn, st sql.Statement) (Result, error) {
	node, err := plan.NewBuilder(db.cat).Build(st)
	if err != nil {
		return Result{}, err
	}
	if _, isSelect := st.(*sql.Select); isSelect {
		return collectRows(tx, node)
	}
	n, err := exec.Execute(tx, db.cat, node)
	if err != nil {
		return Result{}, err
	}
	return Result{Kind: KindAffected, Affected: n}, nil
}

func collectRows(tx *txn.Txn, node plan.Node) (Result, error) {
	op, err := exec.Build(tx, node)
	if err != nil {
		return Result{}, err
	}
	defer op.Close()

	res := Result{Kind: KindRows}
	for _, c := range node.Columns() {
		res.Columns = append(res.Columns, c.Name)
	}
	for {
		r, ok, err := op.Next()
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return res, nil
		}
		res.Rows = append(res.Rows, append([]Value(nil), r...))
	}
}

func (db *DB) explain(st sql.Statement) (Result, error) {
	node, err := plan.NewBuilder(db.cat).Build(st)
	if err != nil {
		return Result{}, err
	}
	return Result{Kind: KindText, Text: strings.TrimRight(plan.Explain(node), "\n")}, nil
}

func (db *DB) describe(name string) (Result, error) {
	t, ok := db.cat.Get(name)
	if !ok {
		return Result{}, fmt.Errorf("no such table: %s", name)
	}
	res := Result{Kind: KindRows, Columns: []string{"column", "type", "null", "key"}}
	for _, c := range t.Visible() {
		null := "yes"
		if c.NotNull {
			null = "no"
		}
		key := ""
		for i, id := range t.PK {
			if id == c.ID {
				key = fmt.Sprintf("pk%d", i+1)
			}
		}
		for i := range t.Indexes {
			idx := &t.Indexes[i]
			for _, id := range idx.Columns {
				if id != c.ID {
					continue
				}
				if key != "" {
					key += ", "
				}
				key += idx.Name
			}
		}
		res.Rows = append(res.Rows, []Value{
			types.NewText(c.Name), types.NewText(c.Type.String()),
			types.NewText(null), types.NewText(key),
		})
	}
	return res, nil
}

// Stats reports storage engine statistics, which is the honest way to see what
// a workload cost: the query layer's job is to turn SQL into key ranges, and
// this is what those ranges did to the tree underneath.
func (db *DB) Stats() lsmdb.Stats { return db.store.Stats() }
