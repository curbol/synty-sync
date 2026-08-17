package browse

import (
	"strings"

	"github.com/curbol/synty-sync/internal/assetindex"
)

// searchQuery is a compiled Google-style search expression evaluated against an
// asset. Whitespace is AND, `OR` (or `|`) alternates, a leading `-` negates,
// `"…"` quotes a literal phrase, `( )` groups, and `field:value` scopes a term to
// one asset field. `OR` binds looser than the implicit AND, so `a b OR c` reads as
// `(a AND b) OR c`. Malformed input never errors: it degrades to a best effort.
type searchQuery struct{ root searchNode }

// parseQuery compiles a raw query string, or returns nil when it holds no terms
// (an all-match). A nil *searchQuery matches every asset.
func parseQuery(s string) *searchQuery {
	toks := tokenize(s)
	if len(toks) == 0 {
		return nil
	}
	p := &parser{toks: toks}
	return &searchQuery{root: p.parseOr()}
}

func (q *searchQuery) match(a assetindex.Asset) bool {
	if q == nil || q.root == nil {
		return true
	}
	return q.root.eval(a)
}

type searchNode interface{ eval(a assetindex.Asset) bool }

type andNode struct{ kids []searchNode }
type orNode struct{ kids []searchNode }
type notNode struct{ kid searchNode }

// termNode is one leaf: a case-insensitive substring test. An empty field scopes
// the match to name, pack, and path; a set field scopes it to that asset field.
type termNode struct {
	field string
	value string
}

func (n andNode) eval(a assetindex.Asset) bool {
	for _, k := range n.kids {
		if !k.eval(a) {
			return false
		}
	}
	return true
}

func (n orNode) eval(a assetindex.Asset) bool {
	for _, k := range n.kids {
		if k.eval(a) {
			return true
		}
	}
	return false
}

func (n notNode) eval(a assetindex.Asset) bool { return !n.kid.eval(a) }

func (n termNode) eval(a assetindex.Asset) bool {
	if n.value == "" {
		return true
	}
	if n.field == "" {
		return strings.Contains(strings.ToLower(a.Name), n.value) ||
			strings.Contains(strings.ToLower(a.Pack), n.value) ||
			strings.Contains(strings.ToLower(a.RelPath), n.value)
	}
	return strings.Contains(strings.ToLower(searchFields[n.field](a)), n.value)
}

// searchFields maps a field:value operator name to the asset field it scopes to.
var searchFields = map[string]func(assetindex.Asset) string{
	"name":    func(a assetindex.Asset) string { return a.Name },
	"pack":    func(a assetindex.Asset) string { return a.Pack },
	"vendor":  func(a assetindex.Asset) string { return a.Vendor },
	"type":    func(a assetindex.Asset) string { return string(a.Category) },
	"variant": func(a assetindex.Asset) string { return a.Variant },
	"ext":     func(a assetindex.Asset) string { return a.Ext },
	"guid":    func(a assetindex.Asset) string { return a.Source.Guid },
	"path":    func(a assetindex.Asset) string { return a.RelPath },
}

type tokKind int

const (
	tokTerm tokKind = iota
	tokOr
	tokLParen
	tokRParen
)

type token struct {
	kind  tokKind
	neg   bool
	field string
	value string
}

func isSpace(c rune) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// tokenize splits a query into terms and operators. Quotes protect their span
// from operator interpretation (spaces, `-`, `:`, `OR`, parens are literal
// inside); a `field:` prefix and a leading `-` are only recognized outside quotes.
func tokenize(s string) []token {
	var toks []token
	r := []rune(s)
	i, n := 0, len(r)
	for i < n {
		switch {
		case isSpace(r[i]):
			i++
			continue
		case r[i] == '(':
			toks = append(toks, token{kind: tokLParen})
			i++
			continue
		case r[i] == ')':
			toks = append(toks, token{kind: tokRParen})
			i++
			continue
		}

		var field string
		var val strings.Builder
		split := false // a field: prefix has been taken
		leadingDash := false
		anyQuote := false
		started := false
		for i < n && !isSpace(r[i]) && r[i] != '(' && r[i] != ')' {
			c := r[i]
			switch {
			case c == '"':
				anyQuote = true
				started = true
				i++
				for i < n && r[i] != '"' {
					val.WriteRune(r[i])
					i++
				}
				if i < n {
					i++ // closing quote
				}
			case !started && c == '-':
				leadingDash = true
				started = true
				i++
			case c == ':' && !split && isSearchField(strings.ToLower(val.String())):
				field = strings.ToLower(val.String())
				val.Reset()
				split = true
				started = true
				i++
			default:
				started = true
				val.WriteRune(c)
				i++
			}
		}

		value := val.String()
		if !anyQuote && !leadingDash && !split && (value == "OR" || value == "|") {
			toks = append(toks, token{kind: tokOr})
			continue
		}
		if leadingDash && field == "" && value == "" {
			value = "-" // a lone '-' is a literal term, not a negation
			leadingDash = false
		}
		toks = append(toks, token{kind: tokTerm, neg: leadingDash, field: field, value: strings.ToLower(value)})
	}
	return toks
}

func isSearchField(name string) bool {
	_, ok := searchFields[name]
	return ok
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() (token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return token{}, false
}

// parseOr and parseAnd return nil for a branch that carries no terms, rather than
// an empty node. An empty andNode evaluates to true (the identity for AND), so a
// half-typed "sword OR " would otherwise compile to "sword OR everything" and match
// the whole library on the way to being finished.
func (p *parser) parseOr() searchNode {
	var kids []searchNode
	if k := p.parseAnd(); k != nil {
		kids = append(kids, k)
	}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tokOr {
			break
		}
		p.pos++ // consume OR
		if k := p.parseAnd(); k != nil {
			kids = append(kids, k)
		}
	}
	switch len(kids) {
	case 0:
		return nil
	case 1:
		return kids[0]
	}
	return orNode{kids: kids}
}

func (p *parser) parseAnd() searchNode {
	var kids []searchNode
	for {
		t, ok := p.peek()
		if !ok || t.kind == tokOr || t.kind == tokRParen {
			break
		}
		if k := p.parsePrimary(); k != nil {
			kids = append(kids, k)
		}
	}
	switch len(kids) {
	case 0:
		return nil
	case 1:
		return kids[0]
	}
	return andNode{kids: kids}
}

func (p *parser) parsePrimary() searchNode {
	t := p.toks[p.pos]
	p.pos++
	if t.kind == tokLParen {
		inner := p.parseOr()
		if nt, ok := p.peek(); ok && nt.kind == tokRParen {
			p.pos++
		}
		return inner // nil when the group held no terms
	}
	if t.kind == tokRParen {
		return nil // unbalanced close; nothing to match on
	}
	var node searchNode = termNode{field: t.field, value: t.value}
	if t.neg {
		node = notNode{kid: node}
	}
	return node
}
