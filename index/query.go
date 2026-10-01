package index

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrBadQuery is returned for empty, invalid, or unsupported search syntax.
var ErrBadQuery = errors.New("invalid query")

// Query language (slice 1):
//
//	field:term    token in that field (value is analyzed like indexed text)
//	term          token in the default field ("message")
//	a AND b       intersection; AND binds tighter than OR
//	a OR b        union
//	( ... )       grouping
//
// NOT and phrase queries are rejected so they are not parsed as something else.

type qKind int

const (
	qTerm qKind = iota
	qAnd
	qOr
)

type qNode struct {
	kind  qKind
	field string
	term  string
	kids  []*qNode
}

func (n *qNode) termCount() int {
	if n == nil {
		return 0
	}
	if n.kind == qTerm {
		return 1
	}
	total := 0
	for _, k := range n.kids {
		total += k.termCount()
	}
	return total
}

func (n *qNode) matches(terms map[string]struct{}) bool {
	switch n.kind {
	case qTerm:
		_, ok := terms[scopedTerm(n.field, n.term)]
		return ok
	case qAnd:
		for _, k := range n.kids {
			if !k.matches(terms) {
				return false
			}
		}
		return true
	case qOr:
		for _, k := range n.kids {
			if k.matches(terms) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func badQueryf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadQuery, fmt.Sprintf(format, args...))
}

type tokKind int

const (
	tokEOF tokKind = iota
	tokTerm
	tokAND
	tokOR
	tokLParen
	tokRParen
)

type token struct {
	kind tokKind
	text string
}

func (t token) String() string {
	switch t.kind {
	case tokAND:
		return "AND"
	case tokOR:
		return "OR"
	case tokLParen:
		return "("
	case tokRParen:
		return ")"
	case tokEOF:
		return "end of query"
	default:
		if t.text == "" {
			return "term"
		}
		return t.text
	}
}

func parseQuery(q string) (*qNode, error) {
	if strings.Contains(q, `"`) {
		return nil, badQueryf("phrase queries are not supported")
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, badQueryf("query is empty")
	}
	toks, err := lex(q)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, badQueryf("query is empty")
	}
	p := &parser{toks: toks}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokEOF {
		return nil, badQueryf("unexpected token %q; expected AND, OR, or end of query", p.peek().String())
	}
	return n, nil
}

func lex(input string) ([]token, error) {
	runes := []rune(input)
	var toks []token
	i := 0
	for i < len(runes) {
		if unicode.IsSpace(runes[i]) {
			i++
			continue
		}
		switch runes[i] {
		case '(':
			toks = append(toks, token{kind: tokLParen, text: "("})
			i++
			continue
		case ')':
			toks = append(toks, token{kind: tokRParen, text: ")"})
			i++
			continue
		}
		start := i
		for i < len(runes) && !unicode.IsSpace(runes[i]) && runes[i] != '(' && runes[i] != ')' {
			i++
		}
		word := string(runes[start:i])
		switch strings.ToUpper(word) {
		case "AND":
			toks = append(toks, token{kind: tokAND, text: word})
		case "OR":
			toks = append(toks, token{kind: tokOR, text: word})
		case "NOT":
			return nil, badQueryf("NOT queries are not supported")
		case "&&", "||":
			return nil, badQueryf("operator %q is not supported; use AND or OR", word)
		default:
			if strings.HasPrefix(word, "-") || strings.HasPrefix(word, "!") {
				return nil, badQueryf("NOT queries are not supported")
			}
			toks = append(toks, token{kind: tokTerm, text: word})
		}
	}
	return toks, nil
}

type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token {
	if p.i >= len(p.toks) {
		return token{kind: tokEOF}
	}
	return p.toks[p.i]
}

func (p *parser) next() token {
	t := p.peek()
	if p.i < len(p.toks) {
		p.i++
	}
	return t
}

func (p *parser) parseOr() (*qNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokOR {
		return left, nil
	}
	node := &qNode{kind: qOr, kids: []*qNode{left}}
	for p.peek().kind == tokOR {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		node.kids = append(node.kids, right)
	}
	return node, nil
}

func (p *parser) parseAnd() (*qNode, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokAND {
		return left, nil
	}
	node := &qNode{kind: qAnd, kids: []*qNode{left}}
	for p.peek().kind == tokAND {
		p.next()
		right, err := p.parsePrimary()
		if err != nil {
			return nil, err
		}
		node.kids = append(node.kids, right)
	}
	return node, nil
}

func (p *parser) parsePrimary() (*qNode, error) {
	t := p.peek()
	switch t.kind {
	case tokLParen:
		p.next()
		if p.peek().kind == tokRParen {
			return nil, badQueryf("empty parentheses")
		}
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokRParen {
			return nil, badQueryf("missing closing parenthesis")
		}
		p.next()
		return n, nil
	case tokTerm:
		p.next()
		return termNode(t.text)
	case tokAND, tokOR:
		return nil, badQueryf("unexpected operator %s", strings.ToUpper(t.text))
	case tokRParen:
		return nil, badQueryf("unexpected closing parenthesis")
	default:
		return nil, badQueryf("unexpected end of query")
	}
}

func termNode(raw string) (*qNode, error) {
	field := DefaultField
	value := raw
	if fieldPart, rest, ok := strings.Cut(raw, ":"); ok {
		if fieldPart == "" {
			return nil, badQueryf("missing field name in %q", raw)
		}
		if strings.HasPrefix(rest, "-") || strings.HasPrefix(rest, "!") {
			return nil, badQueryf("NOT queries are not supported")
		}
		field = fieldPart
		value = rest
	}
	if value == "" {
		return nil, badQueryf("missing term in %q", raw)
	}
	tokens := Analyze(value)
	switch len(tokens) {
	case 0:
		return nil, badQueryf("term %q is below the minimum length of %d", raw, minTokenLen)
	case 1:
		return &qNode{kind: qTerm, field: field, term: tokens[0]}, nil
	default:
		return nil, badQueryf("term %q produces multiple tokens; use AND or OR between single terms", raw)
	}
}
