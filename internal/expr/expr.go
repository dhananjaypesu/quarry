// Package expr is the bound, executable form of a scalar expression.
//
// The parser produces names; this package produces positions. Binding happens
// once per statement, so the executor never looks a column up by name while
// rows are streaming through it — a Column node holds the offset into the
// tuple and reads it directly. That single change is most of the difference
// between an interpreter that walks strings and one that can scan a million
// rows.
package expr

import (
	"fmt"
	"math"
	"strings"

	"github.com/dhananjaypesu/quarry/internal/types"
)

// Expr is a bound scalar expression.
type Expr interface {
	// Eval computes the value of the expression for one input tuple.
	Eval(row []types.Value) (types.Value, error)
	// Type is the static result type, decided at bind time.
	Type() types.Type
	String() string
}

// Column reads a value straight out of the tuple.
type Column struct {
	Pos  int
	Typ  types.Type
	Name string
}

func (c *Column) Eval(row []types.Value) (types.Value, error) { return row[c.Pos], nil }
func (c *Column) Type() types.Type                            { return c.Typ }
func (c *Column) String() string                              { return c.Name }

// Const is a literal, already converted to a runtime value.
type Const struct{ Val types.Value }

func (c *Const) Eval([]types.Value) (types.Value, error) { return c.Val, nil }
func (c *Const) Type() types.Type                        { return c.Val.Type }
func (c *Const) String() string {
	if c.Val.Type == types.Text && !c.Val.Null {
		return "'" + c.Val.S + "'"
	}
	return c.Val.String()
}

// Binary covers arithmetic, comparison, boolean connectives and LIKE.
type Binary struct {
	Op   string
	L, R Expr
	Typ  types.Type
}

func (b *Binary) Type() types.Type { return b.Typ }
func (b *Binary) String() string   { return fmt.Sprintf("(%s %s %s)", b.L, b.Op, b.R) }

func (b *Binary) Eval(row []types.Value) (types.Value, error) {
	// AND and OR short-circuit, which is not merely an optimization: it is
	// what lets "x IS NOT NULL AND 10/x > 1" avoid dividing by zero.
	switch b.Op {
	case "AND":
		l, err := b.L.Eval(row)
		if err != nil {
			return types.Value{}, err
		}
		if lv, known := l.Truthy(); known && !lv {
			return types.NewBool(false), nil
		}
		r, err := b.R.Eval(row)
		if err != nil {
			return types.Value{}, err
		}
		rv, rk := r.Truthy()
		lv, lk := l.Truthy()
		if rk && !rv {
			return types.NewBool(false), nil
		}
		if lk && rk {
			return types.NewBool(lv && rv), nil
		}
		// TRUE AND UNKNOWN is UNKNOWN, and so is UNKNOWN AND UNKNOWN.
		return types.NullOf(types.Bool), nil
	case "OR":
		l, err := b.L.Eval(row)
		if err != nil {
			return types.Value{}, err
		}
		if lv, known := l.Truthy(); known && lv {
			return types.NewBool(true), nil
		}
		r, err := b.R.Eval(row)
		if err != nil {
			return types.Value{}, err
		}
		rv, rk := r.Truthy()
		lv, lk := l.Truthy()
		if rk && rv {
			return types.NewBool(true), nil
		}
		if lk && rk {
			return types.NewBool(lv || rv), nil
		}
		return types.NullOf(types.Bool), nil
	}

	l, err := b.L.Eval(row)
	if err != nil {
		return types.Value{}, err
	}
	r, err := b.R.Eval(row)
	if err != nil {
		return types.Value{}, err
	}
	// Every remaining operator is strict: if either operand is NULL the result
	// is NULL, with no exceptions.
	if l.Null || r.Null {
		return types.NullOf(b.Typ), nil
	}

	switch b.Op {
	case "=", "!=", "<", "<=", ">", ">=":
		c, ok := types.Compare(l, r)
		if !ok {
			return types.NullOf(types.Bool), nil
		}
		return types.NewBool(cmpHolds(b.Op, c)), nil
	case "+", "-", "*", "/", "%":
		return arith(b.Op, l, r)
	case "||":
		return types.NewText(l.String() + r.String()), nil
	case "LIKE":
		return types.NewBool(like(l.S, r.S)), nil
	}
	return types.Value{}, fmt.Errorf("unsupported operator %q", b.Op)
}

func cmpHolds(op string, c int) bool {
	switch op {
	case "=":
		return c == 0
	case "!=":
		return c != 0
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	}
	return false
}

// ErrDivideByZero is returned rather than producing NULL, because silently
// turning an arithmetic fault into a missing value hides real bugs.
var ErrDivideByZero = fmt.Errorf("division by zero")

func arith(op string, l, r types.Value) (types.Value, error) {
	if l.Type == types.Float || r.Type == types.Float {
		a, b := toFloat(l), toFloat(r)
		switch op {
		case "+":
			return types.NewFloat(a + b), nil
		case "-":
			return types.NewFloat(a - b), nil
		case "*":
			return types.NewFloat(a * b), nil
		case "/":
			if b == 0 {
				return types.Value{}, ErrDivideByZero
			}
			return types.NewFloat(a / b), nil
		case "%":
			if b == 0 {
				return types.Value{}, ErrDivideByZero
			}
			return types.NewFloat(math.Mod(a, b)), nil
		}
	}
	if l.Type != types.Int || r.Type != types.Int {
		return types.Value{}, fmt.Errorf("operator %s is not defined on %s and %s", op, l.Type, r.Type)
	}
	switch op {
	case "+":
		return types.NewInt(l.I + r.I), nil
	case "-":
		return types.NewInt(l.I - r.I), nil
	case "*":
		return types.NewInt(l.I * r.I), nil
	case "/":
		if r.I == 0 {
			return types.Value{}, ErrDivideByZero
		}
		return types.NewInt(l.I / r.I), nil
	case "%":
		if r.I == 0 {
			return types.Value{}, ErrDivideByZero
		}
		return types.NewInt(l.I % r.I), nil
	}
	return types.Value{}, fmt.Errorf("unsupported operator %q", op)
}

func toFloat(v types.Value) float64 {
	if v.Type == types.Int {
		return float64(v.I)
	}
	return v.F
}

// like implements SQL's LIKE with % (any run) and _ (any single character).
// It is a straightforward backtracking matcher with a remembered star
// position, which keeps it linear on the patterns people actually write
// instead of exponential like naive recursion.
func like(s, pattern string) bool {
	si, pi := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '_' || pattern[pi] == s[si]):
			si++
			pi++
		case pi < len(pattern) && pattern[pi] == '%':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '%' {
		pi++
	}
	return pi == len(pattern)
}

// Unary is -x or NOT x.
type Unary struct {
	Op  string
	X   Expr
	Typ types.Type
}

func (u *Unary) Type() types.Type { return u.Typ }
func (u *Unary) String() string   { return fmt.Sprintf("%s %s", u.Op, u.X) }

func (u *Unary) Eval(row []types.Value) (types.Value, error) {
	v, err := u.X.Eval(row)
	if err != nil {
		return types.Value{}, err
	}
	if v.Null {
		return types.NullOf(u.Typ), nil
	}
	switch u.Op {
	case "-":
		if v.Type == types.Float {
			return types.NewFloat(-v.F), nil
		}
		if v.Type == types.Int {
			return types.NewInt(-v.I), nil
		}
		return types.Value{}, fmt.Errorf("cannot negate %s", v.Type)
	case "NOT":
		b, known := v.Truthy()
		if !known {
			return types.NullOf(types.Bool), nil
		}
		return types.NewBool(!b), nil
	}
	return types.Value{}, fmt.Errorf("unsupported unary operator %q", u.Op)
}

// IsNull is the one predicate that never returns UNKNOWN.
type IsNull struct {
	X      Expr
	Negate bool
}

func (n *IsNull) Type() types.Type { return types.Bool }
func (n *IsNull) String() string {
	if n.Negate {
		return fmt.Sprintf("(%s IS NOT NULL)", n.X)
	}
	return fmt.Sprintf("(%s IS NULL)", n.X)
}

func (n *IsNull) Eval(row []types.Value) (types.Value, error) {
	v, err := n.X.Eval(row)
	if err != nil {
		return types.Value{}, err
	}
	return types.NewBool(v.Null != n.Negate), nil
}

// In is x IN (list). It follows SQL's rule that a non-match against a list
// containing NULL is UNKNOWN rather than FALSE, because the NULL might have
// been the matching element.
type In struct {
	X      Expr
	List   []Expr
	Negate bool
}

func (i *In) Type() types.Type { return types.Bool }
func (i *In) String() string {
	parts := make([]string, len(i.List))
	for j, e := range i.List {
		parts[j] = e.String()
	}
	op := "IN"
	if i.Negate {
		op = "NOT IN"
	}
	return fmt.Sprintf("(%s %s (%s))", i.X, op, strings.Join(parts, ", "))
}

func (i *In) Eval(row []types.Value) (types.Value, error) {
	x, err := i.X.Eval(row)
	if err != nil {
		return types.Value{}, err
	}
	if x.Null {
		return types.NullOf(types.Bool), nil
	}
	sawNull := false
	for _, e := range i.List {
		v, err := e.Eval(row)
		if err != nil {
			return types.Value{}, err
		}
		if v.Null {
			sawNull = true
			continue
		}
		if c, ok := types.Compare(x, v); ok && c == 0 {
			return types.NewBool(!i.Negate), nil
		}
	}
	if sawNull {
		return types.NullOf(types.Bool), nil
	}
	return types.NewBool(i.Negate), nil
}

// Passes reports whether a predicate admits a row: only a known TRUE does.
func Passes(e Expr, row []types.Value) (bool, error) {
	if e == nil {
		return true, nil
	}
	v, err := e.Eval(row)
	if err != nil {
		return false, err
	}
	b, known := v.Truthy()
	return known && b, nil
}
