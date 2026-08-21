package sql

import (
	"fmt"
	"strings"

	"github.com/dhananjaypesu/quarry/internal/types"
)

// Parse parses a single statement, tolerating a trailing semicolon.
func Parse(src string) (Statement, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: src}
	st, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	p.acceptPunct(";")
	if p.peek().Type != EOF {
		return nil, p.errorf("unexpected %s after statement", p.peek())
	}
	return st, nil
}

// ParseMany splits a script into statements. Splitting happens on the token
// stream rather than on ';' in the raw text, so a semicolon inside a string
// literal does not cut a statement in half.
func ParseMany(src string) ([]Statement, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: src}
	var out []Statement
	for {
		for p.acceptPunct(";") {
		}
		if p.peek().Type == EOF {
			return out, nil
		}
		st, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		out = append(out, st)
		if !p.acceptPunct(";") && p.peek().Type != EOF {
			return nil, p.errorf("expected ; between statements, found %s", p.peek())
		}
	}
}

type parser struct {
	toks []Token
	pos  int
	src  string
}

func (p *parser) peek() Token { return p.toks[p.pos] }
func (p *parser) peekN(n int) Token {
	if p.pos+n >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.pos+n]
}
func (p *parser) advance() Token { t := p.toks[p.pos]; p.pos++; return t }

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("syntax error at position %d: %s", p.peek().Pos, fmt.Sprintf(format, args...))
}

func (p *parser) acceptKeyword(kw ...string) bool {
	t := p.peek()
	if t.Type != Keyword {
		return false
	}
	for _, k := range kw {
		if t.Lit == k {
			p.pos++
			return true
		}
	}
	return false
}

func (p *parser) expectKeyword(kw string) error {
	if !p.acceptKeyword(kw) {
		return p.errorf("expected %s, found %s", kw, p.peek())
	}
	return nil
}

func (p *parser) acceptPunct(s string) bool {
	if t := p.peek(); t.Type == Punct && t.Lit == s {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectPunct(s string) error {
	if !p.acceptPunct(s) {
		return p.errorf("expected %q, found %s", s, p.peek())
	}
	return nil
}

func (p *parser) acceptOp(s string) bool {
	if t := p.peek(); t.Type == Operator && t.Lit == s {
		p.pos++
		return true
	}
	return false
}

// parseIdent accepts an identifier. A handful of keywords double as ordinary
// names in practice (a column called "key" is common), so they are admitted
// here rather than forcing every user to quote them.
func (p *parser) parseIdent() (string, error) {
	t := p.peek()
	if t.Type == Ident {
		p.pos++
		return t.Lit, nil
	}
	if t.Type == Keyword && softKeywords[t.Lit] {
		p.pos++
		return t.Lit, nil
	}
	return "", p.errorf("expected identifier, found %s", t)
}

var softKeywords = map[string]bool{"KEY": true, "TABLES": true, "COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true}

func (p *parser) parseStatement() (Statement, error) {
	t := p.peek()
	if t.Type != Keyword {
		return nil, p.errorf("expected a statement, found %s", t)
	}
	switch t.Lit {
	case "SELECT":
		return p.parseSelect()
	case "INSERT":
		return p.parseInsert()
	case "UPDATE":
		return p.parseUpdate()
	case "DELETE":
		return p.parseDelete()
	case "CREATE":
		return p.parseCreate()
	case "DROP":
		return p.parseDrop()
	case "EXPLAIN":
		p.advance()
		inner, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		return &Explain{Stmt: inner}, nil
	case "BEGIN":
		p.advance()
		return &Begin{}, nil
	case "COMMIT":
		p.advance()
		return &Commit{}, nil
	case "ROLLBACK":
		p.advance()
		return &Rollback{}, nil
	case "SHOW":
		p.advance()
		if err := p.expectKeyword("TABLES"); err != nil {
			return nil, err
		}
		return &ShowTables{}, nil
	case "DESCRIBE":
		p.advance()
		name, err := p.parseIdent()
		if err != nil {
			return nil, err
		}
		return &Describe{Table: name}, nil
	}
	return nil, p.errorf("unsupported statement %s", t)
}

// ---- DDL ----

func (p *parser) parseCreate() (Statement, error) {
	p.advance() // CREATE
	unique := p.acceptKeyword("UNIQUE")
	switch {
	case p.acceptKeyword("TABLE"):
		if unique {
			return nil, p.errorf("UNIQUE is not valid on CREATE TABLE")
		}
		return p.parseCreateTable()
	case p.acceptKeyword("INDEX"):
		return p.parseCreateIndex(unique)
	}
	return nil, p.errorf("expected TABLE or INDEX after CREATE")
}

func (p *parser) parseIfNotExists() bool {
	save := p.pos
	if p.acceptKeyword("IF") && p.acceptKeyword("NOT") && p.acceptKeyword("EXISTS") {
		return true
	}
	p.pos = save
	return false
}

func (p *parser) parseCreateTable() (Statement, error) {
	st := &CreateTable{IfNotExists: p.parseIfNotExists()}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	st.Name = name
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for {
		// A table-level PRIMARY KEY (a, b) clause is the only way to declare a
		// composite key, so it is checked before falling through to a column.
		if p.peek().Type == Keyword && p.peek().Lit == "PRIMARY" && p.peekN(1).Lit == "KEY" {
			p.advance()
			p.advance()
			cols, err := p.parseIdentList()
			if err != nil {
				return nil, err
			}
			st.PrimaryKey = cols
		} else {
			col, err := p.parseColumnDef()
			if err != nil {
				return nil, err
			}
			st.Columns = append(st.Columns, col)
		}
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	if len(st.Columns) == 0 {
		return nil, p.errorf("table %s has no columns", st.Name)
	}
	return st, nil
}

func (p *parser) parseColumnDef() (ColumnDef, error) {
	name, err := p.parseIdent()
	if err != nil {
		return ColumnDef{}, err
	}
	tn, err := p.parseIdent()
	if err != nil {
		return ColumnDef{}, p.errorf("expected a type for column %s", name)
	}
	typ, ok := types.ParseType(tn)
	if !ok {
		return ColumnDef{}, p.errorf("unknown type %q for column %s", tn, name)
	}
	// A parenthesized width such as VARCHAR(64) parses and is ignored: TEXT is
	// variable length, so a declared width carries no information the engine
	// could act on, and rejecting it would break pasted schemas.
	if p.acceptPunct("(") {
		for !p.acceptPunct(")") {
			if p.peek().Type == EOF {
				return ColumnDef{}, p.errorf("unterminated type width")
			}
			p.advance()
		}
	}
	col := ColumnDef{Name: name, Type: typ, TypeName: strings.ToUpper(tn)}
	for {
		switch {
		case p.acceptKeyword("PRIMARY"):
			if err := p.expectKeyword("KEY"); err != nil {
				return ColumnDef{}, err
			}
			col.PrimaryKey = true
			col.NotNull = true
		case p.acceptKeyword("NOT"):
			if err := p.expectKeyword("NULL"); err != nil {
				return ColumnDef{}, err
			}
			col.NotNull = true
		case p.acceptKeyword("NULL"):
			// explicit nullability, the default
		default:
			return col, nil
		}
	}
}

func (p *parser) parseIdentList() ([]string, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var out []string
	for {
		name, err := p.parseIdent()
		if err != nil {
			return nil, err
		}
		out = append(out, name)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *parser) parseCreateIndex(unique bool) (Statement, error) {
	st := &CreateIndex{Unique: unique, IfNotExists: p.parseIfNotExists()}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	st.Name = name
	if err := p.expectKeyword("ON"); err != nil {
		return nil, err
	}
	tbl, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	st.Table = tbl
	cols, err := p.parseIdentList()
	if err != nil {
		return nil, err
	}
	st.Columns = cols
	return st, nil
}

func (p *parser) parseDrop() (Statement, error) {
	p.advance() // DROP
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	st := &DropTable{}
	save := p.pos
	if p.acceptKeyword("IF") && p.acceptKeyword("EXISTS") {
		st.IfExists = true
	} else {
		p.pos = save
	}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	st.Name = name
	return st, nil
}

// ---- DML ----

func (p *parser) parseInsert() (Statement, error) {
	p.advance() // INSERT
	if err := p.expectKeyword("INTO"); err != nil {
		return nil, err
	}
	st := &Insert{}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	st.Table = name
	if p.peek().Type == Punct && p.peek().Lit == "(" {
		cols, err := p.parseIdentList()
		if err != nil {
			return nil, err
		}
		st.Columns = cols
	}
	if err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var row []Expr
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			row = append(row, e)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		st.Rows = append(st.Rows, row)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	return st, nil
}

func (p *parser) parseUpdate() (Statement, error) {
	p.advance() // UPDATE
	st := &Update{}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	st.Table = name
	if err := p.expectKeyword("SET"); err != nil {
		return nil, err
	}
	for {
		col, err := p.parseIdent()
		if err != nil {
			return nil, err
		}
		if !p.acceptOp("=") {
			return nil, p.errorf("expected = in SET clause")
		}
		val, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		st.Set = append(st.Set, Assignment{Column: col, Value: val})
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if p.acceptKeyword("WHERE") {
		if st.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (p *parser) parseDelete() (Statement, error) {
	p.advance() // DELETE
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	st := &Delete{}
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	st.Table = name
	if p.acceptKeyword("WHERE") {
		if st.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// ---- SELECT ----

func (p *parser) parseSelect() (Statement, error) {
	p.advance() // SELECT
	st := &Select{Distinct: p.acceptKeyword("DISTINCT")}
	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		st.Items = append(st.Items, item)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	var err error
	if p.acceptKeyword("FROM") {
		if st.From, err = p.parseTableRef(); err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("WHERE") {
		if st.Where, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("GROUP") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			st.GroupBy = append(st.GroupBy, e)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
	}
	if p.acceptKeyword("HAVING") {
		if st.Having, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			item := OrderItem{Expr: e}
			if p.acceptKeyword("DESC") {
				item.Desc = true
			} else {
				p.acceptKeyword("ASC")
			}
			st.OrderBy = append(st.OrderBy, item)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
	}
	if p.acceptKeyword("LIMIT") {
		if st.Limit, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("OFFSET") {
		if st.Offset, err = p.parseExpr(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (p *parser) parseSelectItem() (SelectItem, error) {
	if p.acceptOp("*") {
		return SelectItem{Star: true}, nil
	}
	// t.* needs three tokens of lookahead-free checking before committing to
	// the general expression path.
	if p.peek().Type == Ident && p.peekN(1).Lit == "." && p.peekN(2).Lit == "*" {
		name := p.advance().Lit
		p.advance()
		p.advance()
		return SelectItem{TableStar: name}, nil
	}
	e, err := p.parseExpr()
	if err != nil {
		return SelectItem{}, err
	}
	item := SelectItem{Expr: e}
	if p.acceptKeyword("AS") {
		alias, err := p.parseIdent()
		if err != nil {
			return SelectItem{}, err
		}
		item.Alias = alias
	} else if p.peek().Type == Ident {
		item.Alias = p.advance().Lit
	}
	return item, nil
}

func (p *parser) parseTableRef() (*TableRef, error) {
	ref, err := p.parseTableName()
	if err != nil {
		return nil, err
	}
	for {
		jt, ok, err := p.parseJoinType()
		if err != nil {
			return nil, err
		}
		if !ok {
			return ref, nil
		}
		right, err := p.parseTableName()
		if err != nil {
			return nil, err
		}
		jc := JoinClause{Type: jt, Right: *right}
		if p.acceptKeyword("ON") {
			if jc.On, err = p.parseExpr(); err != nil {
				return nil, err
			}
		} else if jt != CrossJoin {
			return nil, p.errorf("JOIN requires an ON clause")
		}
		ref.Joins = append(ref.Joins, jc)
	}
}

func (p *parser) parseJoinType() (JoinType, bool, error) {
	switch {
	case p.acceptKeyword("JOIN"):
		return InnerJoin, true, nil
	case p.acceptKeyword("INNER"):
		return InnerJoin, true, p.expectKeyword("JOIN")
	case p.acceptKeyword("CROSS"):
		return CrossJoin, true, p.expectKeyword("JOIN")
	case p.acceptKeyword("LEFT"):
		p.acceptKeyword("OUTER")
		return LeftJoin, true, p.expectKeyword("JOIN")
	}
	return 0, false, nil
}

func (p *parser) parseTableName() (*TableRef, error) {
	name, err := p.parseIdent()
	if err != nil {
		return nil, err
	}
	ref := &TableRef{Name: name}
	if p.acceptKeyword("AS") {
		alias, err := p.parseIdent()
		if err != nil {
			return nil, err
		}
		ref.Alias = alias
	} else if p.peek().Type == Ident {
		ref.Alias = p.advance().Lit
	}
	return ref, nil
}

// ---- expressions ----
//
// Precedence climbing (a Pratt parser without the table indirection): each
// level parses the tighter-binding level below it and then consumes as many
// operators of its own precedence as it can. The result is a left-associative
// tree, which is what SQL's arithmetic and comparison operators require.

func (p *parser) parseExpr() (Expr, error) { return p.parseOr() }

func (p *parser) parseOr() (Expr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("OR") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: "OR", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Expr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("AND") {
		right, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: "AND", Left: left, Right: right}
	}
	return left, nil
}

func (p *parser) parseNot() (Expr, error) {
	if p.acceptKeyword("NOT") {
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "NOT", X: x}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (Expr, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.acceptKeyword("IS"):
			neg := p.acceptKeyword("NOT")
			if err := p.expectKeyword("NULL"); err != nil {
				return nil, err
			}
			left = &IsNullExpr{X: left, Negate: neg}
			continue
		case p.peek().Type == Keyword && p.peek().Lit == "NOT" &&
			(p.peekN(1).Lit == "IN" || p.peekN(1).Lit == "BETWEEN" || p.peekN(1).Lit == "LIKE"):
			p.advance()
			e, err := p.parsePostfixPredicate(left, true)
			if err != nil {
				return nil, err
			}
			left = e
			continue
		case p.peek().Type == Keyword && (p.peek().Lit == "IN" || p.peek().Lit == "BETWEEN" || p.peek().Lit == "LIKE"):
			e, err := p.parsePostfixPredicate(left, false)
			if err != nil {
				return nil, err
			}
			left = e
			continue
		}
		t := p.peek()
		if t.Type != Operator {
			return left, nil
		}
		switch t.Lit {
		case "=", "!=", "<", "<=", ">", ">=":
			p.advance()
			right, err := p.parseAdditive()
			if err != nil {
				return nil, err
			}
			left = &BinaryExpr{Op: t.Lit, Left: left, Right: right}
		default:
			return left, nil
		}
	}
}

func (p *parser) parsePostfixPredicate(left Expr, negate bool) (Expr, error) {
	switch {
	case p.acceptKeyword("IN"):
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		var list []Expr
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			list = append(list, e)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return &InExpr{X: left, List: list, Negate: negate}, nil
	case p.acceptKeyword("BETWEEN"):
		lo, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("AND"); err != nil {
			return nil, err
		}
		hi, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		return &BetweenExpr{X: left, Lo: lo, Hi: hi, Negate: negate}, nil
	case p.acceptKeyword("LIKE"):
		right, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		e := Expr(&BinaryExpr{Op: "LIKE", Left: left, Right: right})
		if negate {
			e = &UnaryExpr{Op: "NOT", X: e}
		}
		return e, nil
	}
	return nil, p.errorf("expected IN, BETWEEN or LIKE")
}

func (p *parser) parseAdditive() (Expr, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Type != Operator || (t.Lit != "+" && t.Lit != "-" && t.Lit != "||") {
			return left, nil
		}
		p.advance()
		right, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: t.Lit, Left: left, Right: right}
	}
}

func (p *parser) parseMultiplicative() (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.Type != Operator || (t.Lit != "*" && t.Lit != "/" && t.Lit != "%") {
			return left, nil
		}
		p.advance()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: t.Lit, Left: left, Right: right}
	}
}

func (p *parser) parseUnary() (Expr, error) {
	if p.acceptOp("-") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &UnaryExpr{Op: "-", X: x}, nil
	}
	p.acceptOp("+")
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (Expr, error) {
	t := p.peek()
	switch t.Type {
	case Number:
		p.advance()
		if t.IsInt {
			return &Literal{Value: types.NewInt(t.Int)}, nil
		}
		return &Literal{Value: types.NewFloat(t.Num)}, nil
	case StringLit:
		p.advance()
		return &Literal{Value: types.NewText(t.Lit)}, nil
	case Keyword:
		switch t.Lit {
		case "NULL":
			p.advance()
			return &Literal{Value: types.NullValue}, nil
		case "TRUE":
			p.advance()
			return &Literal{Value: types.NewBool(true)}, nil
		case "FALSE":
			p.advance()
			return &Literal{Value: types.NewBool(false)}, nil
		case "COUNT", "SUM", "AVG", "MIN", "MAX":
			if p.peekN(1).Lit == "(" {
				return p.parseFuncCall()
			}
		}
	case Ident:
		if p.peekN(1).Type == Punct && p.peekN(1).Lit == "(" {
			return p.parseFuncCall()
		}
		p.advance()
		if p.acceptPunct(".") {
			col, err := p.parseIdent()
			if err != nil {
				return nil, err
			}
			return &ColumnRef{Table: t.Lit, Name: col}, nil
		}
		return &ColumnRef{Name: t.Lit}, nil
	case Punct:
		if t.Lit == "(" {
			p.advance()
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	return nil, p.errorf("expected an expression, found %s", t)
}

func (p *parser) parseFuncCall() (Expr, error) {
	name := strings.ToUpper(p.advance().Lit)
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	fc := &FuncCall{Name: name}
	if p.acceptOp("*") {
		fc.Star = true
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
		return fc, nil
	}
	if p.acceptPunct(")") {
		return fc, nil
	}
	for {
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		fc.Args = append(fc.Args, e)
		if p.acceptPunct(",") {
			continue
		}
		break
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return fc, nil
}
