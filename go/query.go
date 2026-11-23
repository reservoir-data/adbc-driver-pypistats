package pypistats

import (
	"fmt"
	"slices"
	"strings"
)

// parsedQuery is the result of parsing a statement's query text.
type parsedQuery struct {
	// table is the unqualified table name, as written.
	table string
	// packages is non-nil when the query has a package predicate.
	packages []string
}

// queryError distinguishes malformed SQL from SQL this driver can't evaluate.
type queryError struct {
	unsupported bool
	msg         string
}

func (e *queryError) Error() string { return e.msg }

func syntaxErr(format string, args ...any) error {
	return &queryError{msg: fmt.Sprintf(format, args...)}
}

func unsupportedErr(format string, args ...any) error {
	return &queryError{unsupported: true, msg: fmt.Sprintf(format, args...)}
}

type tokenKind int

const (
	tokIdent  tokenKind = iota // bare or "quoted" (possibly dotted) identifier
	tokString                  // 'string literal'
	tokPunct                   // one of ( ) , = * ;
)

type token struct {
	kind tokenKind
	text string // identifier path / string value / punctuation
	// bare is true for an unquoted identifier, i.e. one that may be a keyword.
	bare bool
}

// tokenize splits q into identifiers, string literals and punctuation. Dotted
// names such as pypistats.api.system become a single identifier token holding
// the last segment, since catalog and schema are fixed.
func tokenize(q string) ([]token, error) {
	var toks []token
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case strings.IndexByte("(),=*;", c) >= 0:
			toks = append(toks, token{kind: tokPunct, text: string(c)})
			i++
		case c == '\'':
			var sb strings.Builder
			i++
			for {
				if i >= len(q) {
					return nil, syntaxErr("unterminated string literal")
				}
				if q[i] == '\'' {
					if i+1 < len(q) && q[i+1] == '\'' {
						sb.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				sb.WriteByte(q[i])
				i++
			}
			toks = append(toks, token{kind: tokString, text: sb.String()})
		case c == '"' || c == '_' || c == '.' || isAlnum(c):
			var last string
			bare := true
			for {
				var seg string
				switch {
				case i < len(q) && q[i] == '"':
					end := strings.IndexByte(q[i+1:], '"')
					if end < 0 {
						return nil, syntaxErr("unterminated quoted identifier")
					}
					seg = q[i+1 : i+1+end]
					i += end + 2
					bare = false
				default:
					start := i
					for i < len(q) && (q[i] == '_' || isAlnum(q[i])) {
						i++
					}
					seg = q[start:i]
				}
				last = seg
				if i < len(q) && q[i] == '.' {
					i++
					bare = false
					continue
				}
				break
			}
			if last == "" {
				return nil, syntaxErr("unexpected %q", string(c))
			}
			toks = append(toks, token{kind: tokIdent, text: last, bare: bare})
		default:
			return nil, syntaxErr("unexpected character %q", string(c))
		}
	}
	return toks, nil
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() *token {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func (p *parser) next() *token {
	t := p.peek()
	if t != nil {
		p.pos++
	}
	return t
}

func (p *parser) isKeyword(kw string) bool {
	t := p.peek()
	return t != nil && t.kind == tokIdent && t.bare && strings.EqualFold(t.text, kw)
}

func (p *parser) acceptKeyword(kw string) bool {
	if p.isKeyword(kw) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) acceptPunct(s string) bool {
	if t := p.peek(); t != nil && t.kind == tokPunct && t.text == s {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectKeyword(kw string) error {
	if !p.acceptKeyword(kw) {
		return syntaxErr("expected %s", strings.ToUpper(kw))
	}
	return nil
}

func (p *parser) expectPunct(s string) error {
	if !p.acceptPunct(s) {
		return syntaxErr("expected %q", s)
	}
	return nil
}

func (p *parser) expectString() (string, error) {
	t := p.next()
	if t == nil || t.kind != tokString {
		return "", syntaxErr("expected a string literal")
	}
	return t.text, nil
}

// parseQuery accepts
//
//	<table>
//	SELECT * FROM <table> [WHERE package = '<pkg>' | package IN ('<pkg>', ...)] [;]
//
// Table names may be qualified (pypistats.api.<table>) and are matched
// case-insensitively. Any other SQL is rejected: ignoring a clause the driver
// can't evaluate would silently return the wrong rows.
func parseQuery(q string) (parsedQuery, error) {
	toks, err := tokenize(q)
	if err != nil {
		return parsedQuery{}, err
	}
	if n := len(toks); n > 0 && toks[n-1].kind == tokPunct && toks[n-1].text == ";" {
		toks = toks[:n-1]
	}
	p := &parser{toks: toks}

	var out parsedQuery
	if len(toks) == 1 && toks[0].kind == tokIdent {
		out.table = toks[0].text
		return out, nil
	}

	if err := p.expectKeyword("select"); err != nil {
		return out, err
	}
	if !p.acceptPunct("*") {
		return out, unsupportedErr("only SELECT * is supported")
	}
	if err := p.expectKeyword("from"); err != nil {
		return out, err
	}
	t := p.next()
	if t == nil || t.kind != tokIdent {
		return out, syntaxErr("expected a table name")
	}
	out.table = t.text

	if p.peek() == nil {
		return out, nil
	}
	if err := p.expectKeyword("where"); err != nil {
		return out, unsupportedErr("only a WHERE clause is supported after the table name")
	}
	pkgs, err := p.parsePackagePredicate()
	if err != nil {
		return out, err
	}
	if p.peek() != nil {
		if p.isKeyword("and") || p.isKeyword("or") {
			return out, unsupportedErr("only a single package predicate is supported; use package IN (...) for several packages")
		}
		return out, unsupportedErr("unsupported clause after the package predicate")
	}
	out.packages = pkgs
	return out, nil
}

func (p *parser) parsePackagePredicate() ([]string, error) {
	col := p.next()
	if col == nil || col.kind != tokIdent {
		return nil, syntaxErr("expected a column name after WHERE")
	}
	if !strings.EqualFold(col.text, "package") {
		return nil, unsupportedErr("cannot filter on column %q; only the package column is supported", col.text)
	}

	var pkgs []string
	switch {
	case p.acceptPunct("="):
		s, err := p.expectString()
		if err != nil {
			return nil, err
		}
		pkgs = []string{s}
	case p.acceptKeyword("in"):
		if err := p.expectPunct("("); err != nil {
			return nil, err
		}
		for {
			s, err := p.expectString()
			if err != nil {
				return nil, err
			}
			pkgs = append(pkgs, s)
			if p.acceptPunct(",") {
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	default:
		return nil, unsupportedErr("only = and IN are supported on package")
	}

	pkgs = slices.DeleteFunc(pkgs, func(s string) bool { return strings.TrimSpace(s) == "" })
	if len(pkgs) == 0 {
		return nil, syntaxErr("package predicate has no package names")
	}
	// Preserve order, drop duplicates so each package is fetched once.
	out := pkgs[:0:0]
	for _, s := range pkgs {
		s = strings.TrimSpace(s)
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}
