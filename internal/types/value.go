// Package types defines the runtime value domain of the engine: the four
// column types, the single boxed Value that flows through every operator, and
// SQL's three-valued comparison rules.
//
// A value is a struct rather than an interface. Every tuple produced by a scan
// allocates one Value per column, and an interface would put each of those on
// the heap; a flat struct keeps a whole row in one slice backing array. The
// cost is 40 bytes per value regardless of type, which is the right trade when
// rows are read in batches and discarded.
package types

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Type is a declared column type.
type Type uint8

const (
	// Unknown is the type of a NULL literal before it is coerced to a column.
	Unknown Type = iota
	Bool
	Int
	Float
	Text
)

func (t Type) String() string {
	switch t {
	case Bool:
		return "BOOL"
	case Int:
		return "INT"
	case Float:
		return "FLOAT"
	case Text:
		return "TEXT"
	}
	return "UNKNOWN"
}

// ParseType maps a SQL type name to a Type. The aliases exist because the
// parser accepts the spellings people actually type.
func ParseType(name string) (Type, bool) {
	switch strings.ToUpper(name) {
	case "BOOL", "BOOLEAN":
		return Bool, true
	case "INT", "INTEGER", "BIGINT", "SMALLINT":
		return Int, true
	case "FLOAT", "DOUBLE", "REAL":
		return Float, true
	case "TEXT", "STRING", "VARCHAR", "CHAR":
		return Text, true
	}
	return Unknown, false
}

// Value is a single SQL datum. The zero Value is NULL, which matters: a row
// decoded from storage with a missing column is correctly NULL without any
// explicit initialization.
type Value struct {
	Type Type
	// Null is tracked separately from Type because a NULL still carries the
	// type of the column it came from, and losing that would break comparison
	// against a typed operand.
	Null bool
	I    int64
	F    float64
	S    string
	B    bool
}

// Null is the untyped NULL produced by the literal NULL.
var NullValue = Value{Type: Unknown, Null: true}

func NullOf(t Type) Value  { return Value{Type: t, Null: true} }
func NewInt(i int64) Value { return Value{Type: Int, I: i} }
func NewFloat(f float64) Value {
	return Value{Type: Float, F: f}
}
func NewText(s string) Value { return Value{Type: Text, S: s} }
func NewBool(b bool) Value   { return Value{Type: Bool, B: b} }

// IsNull reports whether v is SQL NULL.
func (v Value) IsNull() bool { return v.Null }

// Truthy collapses a value to SQL's three-valued logic, returning the truth
// value and whether it is known. Only a known TRUE passes a WHERE clause; both
// FALSE and UNKNOWN filter the row out, which is why callers must not treat
// "not true" as "false" when propagating through NOT.
func (v Value) Truthy() (val bool, known bool) {
	if v.Null {
		return false, false
	}
	switch v.Type {
	case Bool:
		return v.B, true
	case Int:
		return v.I != 0, true
	case Float:
		return v.F != 0, true
	case Text:
		return v.S != "", true
	}
	return false, false
}

// String renders a value the way the shell prints it.
func (v Value) String() string {
	if v.Null {
		return "NULL"
	}
	switch v.Type {
	case Bool:
		return strconv.FormatBool(v.B)
	case Int:
		return strconv.FormatInt(v.I, 10)
	case Float:
		return strconv.FormatFloat(v.F, 'g', -1, 64)
	case Text:
		return v.S
	}
	return "?"
}

// Compare orders two values, returning -1, 0 or 1. The second result is false
// when the comparison is UNKNOWN because either side is NULL — SQL does not
// let NULL compare equal to anything, including itself.
//
// Ordering across types only happens for numeric mixes (INT against FLOAT),
// which are compared as floats. Any other cross-type comparison is a type
// error caught by the binder before execution, so it cannot reach here.
func Compare(a, b Value) (int, bool) {
	if a.Null || b.Null {
		return 0, false
	}
	if a.Type == Int && b.Type == Float {
		return cmpFloat(float64(a.I), b.F), true
	}
	if a.Type == Float && b.Type == Int {
		return cmpFloat(a.F, float64(b.I)), true
	}
	switch a.Type {
	case Int:
		switch {
		case a.I < b.I:
			return -1, true
		case a.I > b.I:
			return 1, true
		}
		return 0, true
	case Float:
		return cmpFloat(a.F, b.F), true
	case Text:
		return strings.Compare(a.S, b.S), true
	case Bool:
		switch {
		case !a.B && b.B:
			return -1, true
		case a.B && !b.B:
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	case math.IsNaN(a) && !math.IsNaN(b):
		return -1
	case !math.IsNaN(a) && math.IsNaN(b):
		return 1
	}
	return 0
}

// CompareNullsFirst is a total order used by ORDER BY and by grouping, where
// NULLs must sort somewhere deterministic rather than being unordered. NULL
// sorts before every non-NULL value, matching SQLite and PostgreSQL ASC.
func CompareNullsFirst(a, b Value) int {
	switch {
	case a.Null && b.Null:
		return 0
	case a.Null:
		return -1
	case b.Null:
		return 1
	}
	c, _ := Compare(a, b)
	return c
}

// Cast converts a value to a column's declared type on the way into storage.
// Only widening and lossless-looking conversions are allowed: an INT fits a
// FLOAT column, but a TEXT does not silently become an INT, because a typo in
// a literal should be an error rather than a zero row.
func Cast(v Value, t Type) (Value, error) {
	if v.Null {
		return NullOf(t), nil
	}
	if v.Type == t {
		return v, nil
	}
	if v.Type == Int && t == Float {
		return NewFloat(float64(v.I)), nil
	}
	return Value{}, fmt.Errorf("cannot store %s value into %s column", v.Type, t)
}
