// Package sql is the front end: it turns SQL text into an abstract syntax
// tree. Nothing in this package knows about storage, values, or execution —
// it deals only in the shape of the language, which keeps the grammar
// readable and lets the binder own every semantic decision.
package sql

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// TokenType classifies a lexeme.
type TokenType uint8

const (
	EOF TokenType = iota
	Ident
	Keyword
	Number
	StringLit
	Operator
	Punct
)

// Token is a lexeme plus the byte offset where it started, which is what lets
// a syntax error point at the offending character instead of the statement.
type Token struct {
	Type  TokenType
	Lit   string  // literal text, upper-cased for keywords
	Num   float64 // set for Number
	Int   int64   // set for Number when it is integral
	IsInt bool
	Pos   int
}

func (t Token) String() string {
	if t.Type == EOF {
		return "end of statement"
	}
	return strconv.Quote(t.Lit)
}

// keywords are recognized case-insensitively. Identifiers that collide with a
// keyword must be double-quoted, the same rule SQL itself uses.
var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "INSERT": true, "INTO": true,
	"VALUES": true, "UPDATE": true, "SET": true, "DELETE": true, "CREATE": true,
	"TABLE": true, "DROP": true, "INDEX": true, "ON": true, "AND": true,
	"OR": true, "NOT": true, "NULL": true, "IS": true, "PRIMARY": true,
	"KEY": true, "ORDER": true, "BY": true, "ASC": true, "DESC": true,
	"LIMIT": true, "OFFSET": true, "AS": true, "JOIN": true, "INNER": true,
	"LEFT": true, "GROUP": true, "HAVING": true, "DISTINCT": true, "TRUE": true,
	"FALSE": true, "UNIQUE": true, "IF": true, "EXISTS": true, "BEGIN": true,
	"COMMIT": true, "ROLLBACK": true, "EXPLAIN": true, "COUNT": true,
	"SUM": true, "AVG": true, "MIN": true, "MAX": true, "LIKE": true,
	"IN": true, "BETWEEN": true, "OUTER": true, "CROSS": true, "SHOW": true,
	"TABLES": true, "DESCRIBE": true,
}

type lexer struct {
	src  string
	pos  int
	toks []Token
}

// Lex tokenizes a statement. It fails fast on unterminated strings and stray
// characters rather than producing a token stream the parser has to defend
// against.
func Lex(src string) ([]Token, error) {
	l := &lexer{src: src}
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		l.toks = append(l.toks, t)
		if t.Type == EOF {
			return l.toks, nil
		}
	}
}

func (l *lexer) next() (Token, error) {
	l.skipSpaceAndComments()
	if l.pos >= len(l.src) {
		return Token{Type: EOF, Pos: l.pos}, nil
	}
	start := l.pos
	c := l.src[l.pos]

	switch {
	case c == '\'':
		return l.lexString(start)
	case c == '"':
		return l.lexQuotedIdent(start)
	case isDigit(c) || (c == '.' && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1])):
		return l.lexNumber(start)
	case isIdentStart(c):
		return l.lexIdent(start)
	}

	// Two-character operators are checked before single ones so that "<=" is
	// never read as "<" followed by "=".
	if l.pos+1 < len(l.src) {
		two := l.src[l.pos : l.pos+2]
		switch two {
		case "<=", ">=", "!=", "<>", "||":
			l.pos += 2
			if two == "<>" {
				two = "!="
			}
			return Token{Type: Operator, Lit: two, Pos: start}, nil
		}
	}
	switch c {
	case '+', '-', '*', '/', '%', '=', '<', '>':
		l.pos++
		return Token{Type: Operator, Lit: string(c), Pos: start}, nil
	case '(', ')', ',', ';', '.':
		l.pos++
		return Token{Type: Punct, Lit: string(c), Pos: start}, nil
	}
	return Token{}, fmt.Errorf("unexpected character %q at position %d", string(c), start)
}

func (l *lexer) skipSpaceAndComments() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if unicode.IsSpace(rune(c)) {
			l.pos++
			continue
		}
		if c == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-' {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		return
	}
}

func (l *lexer) lexString(start int) (Token, error) {
	l.pos++ // opening quote
	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\'' {
			// SQL escapes a quote by doubling it, so '' inside a string is one
			// literal quote and not the end of the string.
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'' {
				sb.WriteByte('\'')
				l.pos += 2
				continue
			}
			l.pos++
			return Token{Type: StringLit, Lit: sb.String(), Pos: start}, nil
		}
		sb.WriteByte(c)
		l.pos++
	}
	return Token{}, fmt.Errorf("unterminated string starting at position %d", start)
}

func (l *lexer) lexQuotedIdent(start int) (Token, error) {
	l.pos++
	var sb strings.Builder
	for l.pos < len(l.src) {
		if l.src[l.pos] == '"' {
			l.pos++
			return Token{Type: Ident, Lit: sb.String(), Pos: start}, nil
		}
		sb.WriteByte(l.src[l.pos])
		l.pos++
	}
	return Token{}, fmt.Errorf("unterminated quoted identifier at position %d", start)
}

func (l *lexer) lexNumber(start int) (Token, error) {
	isFloat := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if isDigit(c) {
			l.pos++
			continue
		}
		if c == '.' && !isFloat {
			isFloat = true
			l.pos++
			continue
		}
		if (c == 'e' || c == 'E') && l.pos+1 < len(l.src) {
			isFloat = true
			l.pos++
			if l.src[l.pos] == '+' || l.src[l.pos] == '-' {
				l.pos++
			}
			continue
		}
		break
	}
	lit := l.src[start:l.pos]
	if !isFloat {
		i, err := strconv.ParseInt(lit, 10, 64)
		if err == nil {
			return Token{Type: Number, Lit: lit, Int: i, IsInt: true, Num: float64(i), Pos: start}, nil
		}
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return Token{}, fmt.Errorf("malformed number %q at position %d", lit, start)
	}
	return Token{Type: Number, Lit: lit, Num: f, Pos: start}, nil
}

func (l *lexer) lexIdent(start int) (Token, error) {
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.pos++
	}
	lit := l.src[start:l.pos]
	up := strings.ToUpper(lit)
	if keywords[up] {
		return Token{Type: Keyword, Lit: up, Pos: start}, nil
	}
	return Token{Type: Ident, Lit: lit, Pos: start}, nil
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || unicode.IsLetter(rune(c)) }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }
