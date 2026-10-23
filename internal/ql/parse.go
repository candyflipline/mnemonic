package ql

import (
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokKind uint8

const (
	tEOF tokKind = iota
	tIdent
	tString
	tInt
	tFloat
	tPunct
	tErr
)

type token struct {
	kind     tokKind
	text     string
	at       Pos
	off, end int
}

const clauseNames = "from, select, filter, dimensions, measures, sort, limit"

var reserved = map[string]bool{
	"and": true, "or": true, "not": true, "in": true, "is": true, "null": true, "like": true, "ilike": true,
	"match": true, "includes": true, "as": true, "asc": true, "desc": true, "true": true, "false": true,
}

var sqlWords = map[string]bool{
	"select": true, "with": true, "insert": true, "update": true, "delete": true, "create": true, "drop": true,
	"alter": true, "copy": true, "attach": true, "detach": true, "pragma": true, "set": true, "explain": true,
	"describe": true, "show": true, "call": true, "install": true, "load": true, "export": true, "import": true,
	"values": true, "table": true, "summarize": true, "begin": true, "commit": true, "rollback": true,
}

var puncts = []string{"!=", "<>", "<=", ">=", "|", ":", ",", "(", ")", "[", "]", "*", "+", "-", "/", "=", "<", ">"}

func lex(src string) []token {
	var toks []token
	line, col := 1, 1
	i := 0
	advance := func(to int) {
		for i < to {
			r, n := utf8.DecodeRuneInString(src[i:])
			i += n
			if r == '\n' {
				line, col = line+1, 1
			} else {
				col++
			}
		}
	}
	for {
		for i < len(src) {
			r, _ := utf8.DecodeRuneInString(src[i:])
			if !unicode.IsSpace(r) {
				break
			}
			advance(i + 1)
		}
		at, start := Pos{line, col}, i
		if i == len(src) {
			return append(toks, token{kind: tEOF, at: at, off: i, end: i})
		}
		c := src[i]
		emit := func(k tokKind, text string, end int) {
			toks = append(toks, token{kind: k, text: text, at: at, off: start, end: end})
			advance(end)
		}
		fail := func(msg string) []token {
			return append(toks, token{kind: tErr, text: msg, at: at, off: start, end: start})
		}
		switch {
		case c == '_' || c < utf8.RuneSelf && unicode.IsLetter(rune(c)):
			j := i
			for j < len(src) && (src[j] == '_' || src[j] < utf8.RuneSelf && (unicode.IsLetter(rune(src[j])) || unicode.IsDigit(rune(src[j])))) {
				j++
			}
			emit(tIdent, strings.ToLower(src[i:j]), j)
		case c >= '0' && c <= '9':
			j, kind := i, tInt
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j+1 < len(src) && src[j] == '.' && src[j+1] >= '0' && src[j+1] <= '9' {
				kind = tFloat
				for j++; j < len(src) && src[j] >= '0' && src[j] <= '9'; j++ {
				}
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				k := j + 1
				if k < len(src) && (src[k] == '+' || src[k] == '-') {
					k++
				}
				if k < len(src) && src[k] >= '0' && src[k] <= '9' {
					kind = tFloat
					for j = k; j < len(src) && src[j] >= '0' && src[j] <= '9'; j++ {
					}
				}
			}
			emit(kind, src[i:j], j)
		case c == '\'':
			var b strings.Builder
			j := i + 1
			for {
				k := strings.IndexByte(src[j:], '\'')
				if k < 0 {
					return fail("the string starting here has no closing '")
				}
				b.WriteString(src[j : j+k])
				j += k + 1
				if j < len(src) && src[j] == '\'' {
					b.WriteByte('\'')
					j++
					continue
				}
				break
			}
			emit(tString, b.String(), j)
		case c == '"':
			return fail(`strings use single quotes: 'text'`)
		default:
			k := slices.IndexFunc(puncts, func(p string) bool { return strings.HasPrefix(src[start:], p) })
			if k < 0 {
				r, _ := utf8.DecodeRuneInString(src[start:])
				return fail("unexpected character " + strconv.QuoteRune(r))
			}
			text := puncts[k]
			if text == "<>" {
				text = "!="
			}
			emit(tPunct, text, start+len(puncts[k]))
		}
	}
}

type parser struct {
	src  string
	toks []token
	i    int
}

func Parse(src string) (q Query, err error) {
	p := &parser{src: src, toks: lex(src)}
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*Error)
			if !ok {
				panic(r)
			}
			q, err = Query{}, e
		}
	}()
	return p.query(), nil
}

func (p *parser) peek() token {
	t := p.toks[p.i]
	if t.kind == tErr {
		panic(&Error{At: t.at, Msg: t.text})
	}
	return t
}

func (p *parser) next() token {
	t := p.peek()
	if t.kind != tEOF {
		p.i++
	}
	return t
}

func (p *parser) raw(i int) token {
	if i < len(p.toks) {
		return p.toks[i]
	}
	return p.toks[len(p.toks)-1]
}

func (t token) is(kind tokKind, text string) bool { return t.kind == kind && t.text == text }

func (t token) kw(word string) bool { return t.is(tIdent, word) }

func (t token) punct(s string) bool { return t.is(tPunct, s) }

func describe(t token) string {
	switch t.kind {
	case tEOF:
		return "the end of the query"
	case tString:
		return strconv.Quote(t.text)
	}
	return "\"" + t.text + "\""
}

func (p *parser) fail(t token, format string, args ...any) {
	panic(errorf(t.at, format, args...))
}

func (p *parser) expectPunct(s string) token {
	t := p.next()
	if !t.punct(s) {
		p.fail(t, "expected %q, got %s", s, describe(t))
	}
	return t
}

func (p *parser) clauseStart(i int) bool {
	return p.raw(i).kind == tIdent && p.raw(i+1).punct(":")
}

func (p *parser) query() Query {
	var q Query
	seen := map[string]Pos{}
	var shapeAt Pos
	var dims, measures []Item
	for first := true; p.peek().kind != tEOF; first = false {
		t := p.peek()
		if t.punct("|") {
			p.next()
			continue
		}
		if !p.clauseStart(p.i) {
			if first && t.kind == tIdent && sqlWords[t.text] {
				p.fail(t, "SQL is not accepted; write clauses, for example: from: events | filter: harness = 'codex' | select: session_id, event_ts")
			}
			p.fail(t, "expected a clause name (%s) and ':', got %s", clauseNames, describe(t))
		}
		if prev, ok := seen[t.text]; ok {
			p.fail(t, "the %s clause is given twice (first at %s)", t.text, prev)
		}
		seen[t.text] = t.at
		p.i += 2
		switch t.text {
		case "from":
			q.From = p.ident("a relation name")
		case "select":
			if p.peek().punct("*") {
				q.Shape = SelectAll{At: p.next().at}
			} else {
				q.Shape = Select{Items: p.items()}
			}
			shapeAt = t.at
		case "filter":
			q.Filter = p.expr()
		case "dimensions":
			dims = p.items()
		case "measures":
			measures = p.items()
		case "sort":
			q.Sort = p.sortKeys()
		case "limit":
			n := p.next()
			v, err := strconv.Atoi(n.text)
			if n.kind != tInt || err != nil || v < 1 {
				p.fail(n, "limit takes a positive whole number, got %s", describe(n))
			}
			q.Limit = v
		default:
			p.fail(t, "unknown clause %q; clauses are %s", t.text, clauseNames)
		}
		if after := p.peek(); after.kind != tEOF && !after.punct("|") && !p.clauseStart(p.i) {
			p.fail(after, "unexpected %s in the %s clause", describe(after), t.text)
		}
	}
	if _, ok := seen["from"]; !ok {
		p.fail(p.toks[0], "a query needs a from: clause naming the relation to read")
	}
	if dims != nil || measures != nil {
		if q.Shape != nil {
			at := seen["dimensions"]
			if at == (Pos{}) {
				at = seen["measures"]
			}
			if shapeAt.Line > at.Line || shapeAt.Line == at.Line && shapeAt.Col > at.Col {
				at = shapeAt
			}
			panic(errorf(at, "select returns rows and dimensions/measures return groups; use one or the other"))
		}
		q.Shape = Group{Dimensions: dims, Measures: measures}
	}
	if q.Shape == nil {
		q.Shape = SelectAll{At: seen["from"]}
	}
	return q
}

func (p *parser) ident(what string) Ident {
	t := p.next()
	if t.kind != tIdent || reserved[t.text] {
		p.fail(t, "expected %s, got %s", what, describe(t))
	}
	return Ident{At: t.at, Name: t.text}
}

func (p *parser) items() []Item {
	var out []Item
	for {
		start := p.peek().off
		e := p.expr()
		name := strings.TrimSpace(p.src[start:p.toks[p.i-1].end])
		if c, ok := e.(*Col); ok {
			name = c.Name
		}
		if p.peek().kw("as") {
			p.next()
			name = p.ident("a name after as").Name
		}
		out = append(out, Item{Expr: e, Name: name})
		if !p.peek().punct(",") {
			return out
		}
		p.next()
	}
}

func (p *parser) sortKeys() []SortKey {
	var out []SortKey
	for {
		k := SortKey{Expr: p.expr()}
		if t := p.peek(); t.kw("asc") || t.kw("desc") {
			k.Desc = p.next().text == "desc"
		}
		out = append(out, k)
		if !p.peek().punct(",") {
			return out
		}
		p.next()
	}
}

func (p *parser) expr() Expr {
	l := p.and()
	for p.peek().kw("or") {
		at := p.next().at
		l = &Binary{At: at, Op: OpOr, L: l, R: p.and()}
	}
	return l
}

func (p *parser) and() Expr {
	l := p.not()
	for p.peek().kw("and") {
		at := p.next().at
		l = &Binary{At: at, Op: OpAnd, L: l, R: p.not()}
	}
	return l
}

func (p *parser) not() Expr {
	if t := p.peek(); t.kw("not") {
		p.next()
		return &Unary{At: t.at, Op: OpNot, X: p.not()}
	}
	return p.pred()
}

var compareOps = map[string]Op{"=": OpEq, "!=": OpNe, "<": OpLt, "<=": OpLe, ">": OpGt, ">=": OpGe}

func (p *parser) pred() Expr {
	l := p.sum()
	t := p.peek()
	if op, ok := compareOps[t.text]; ok && t.kind == tPunct {
		p.next()
		return &Binary{At: t.at, Op: op, L: l, R: p.sum()}
	}
	neg := false
	if t.kw("not") {
		if n := p.raw(p.i + 1); !n.kw("in") && !n.kw("like") && !n.kw("ilike") {
			p.fail(n, "expected in, like or ilike after not, got %s", describe(n))
		}
		p.next()
		neg, t = true, p.peek()
	}
	switch {
	case t.kw("in"):
		p.next()
		p.expectPunct("(")
		in := &In{At: t.at, X: l, Not: neg}
		for {
			v := p.unary()
			lit, ok := v.(*Lit)
			if !ok {
				p.fail(p.toks[p.i-1], "in (...) takes literal values only")
			}
			in.List = append(in.List, *lit)
			if !p.peek().punct(",") {
				break
			}
			p.next()
		}
		p.expectPunct(")")
		return in
	case t.kw("like"), t.kw("ilike"):
		p.next()
		return &Like{At: t.at, X: l, Pattern: p.str(t.text), Fold: t.text == "ilike", Not: neg}
	case t.kw("match"):
		c, ok := l.(*Col)
		if !ok {
			p.fail(t, "match takes a column on its left")
		}
		p.next()
		return &Match{At: t.at, Col: *c, Text: p.str("match")}
	case t.kw("includes"):
		p.next()
		return &Includes{At: t.at, X: l, Elem: p.str("includes")}
	case t.kw("is"):
		p.next()
		n := &IsNull{At: t.at, X: l}
		if p.peek().kw("not") {
			p.next()
			n.Not = true
		}
		if v := p.next(); !v.kw("null") {
			p.fail(v, "expected null after is, got %s", describe(v))
		}
		return n
	}
	return l
}

func (p *parser) str(after string) string {
	t := p.next()
	if t.kind != tString {
		p.fail(t, "%s takes a 'string', got %s", after, describe(t))
	}
	return t.text
}

func (p *parser) sum() Expr {
	l := p.product()
	for t := p.peek(); t.punct("+") || t.punct("-"); t = p.peek() {
		p.next()
		l = &Binary{At: t.at, Op: Op(t.text), L: l, R: p.product()}
	}
	return l
}

func (p *parser) product() Expr {
	l := p.unary()
	for t := p.peek(); t.punct("*") || t.punct("/"); t = p.peek() {
		p.next()
		l = &Binary{At: t.at, Op: Op(t.text), L: l, R: p.unary()}
	}
	return l
}

func (p *parser) unary() Expr {
	t := p.peek()
	if !t.punct("-") {
		return p.postfix()
	}
	p.next()
	if n := p.peek(); n.kind == tInt || n.kind == tFloat {
		p.next()
		return p.number(n, "-"+n.text, t.at)
	}
	return &Unary{At: t.at, Op: OpNeg, X: p.unary()}
}

func (p *parser) number(t token, text string, at Pos) *Lit {
	if t.kind == tInt {
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			p.fail(t, "the number %s is out of range", text)
		}
		return &Lit{At: at, Val: IntValue(n)}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		p.fail(t, "the number %s is out of range", text)
	}
	return &Lit{At: at, Val: FloatValue(f)}
}

func (p *parser) postfix() Expr {
	e := p.primary()
	if t := p.peek(); t.punct("[") {
		c, ok := e.(*Col)
		if !ok {
			p.fail(t, "only a map column takes ['key']")
		}
		p.next()
		key := p.str("[")
		p.expectPunct("]")
		return &Index{At: t.at, Col: *c, Key: key}
	}
	return e
}

func (p *parser) primary() Expr {
	t := p.next()
	switch {
	case t.kind == tString:
		return &Lit{At: t.at, Val: StringValue(t.text)}
	case t.kind == tInt || t.kind == tFloat:
		return p.number(t, t.text, t.at)
	case t.kw("true"), t.kw("false"):
		return &Lit{At: t.at, Val: BoolValue(t.text == "true")}
	case t.punct("("):
		e := p.expr()
		p.expectPunct(")")
		return e
	case t.kind == tIdent && !reserved[t.text]:
		if !p.peek().punct("(") {
			return &Col{At: t.at, Name: t.text}
		}
		p.next()
		c := &Call{At: t.at, Name: t.text}
		if p.peek().punct("*") {
			p.next()
			c.Star = true
		} else if !p.peek().punct(")") {
			for {
				c.Args = append(c.Args, p.expr())
				if !p.peek().punct(",") {
					break
				}
				p.next()
			}
		}
		p.expectPunct(")")
		return c
	case t.kw("null"):
		p.fail(t, "null is written only as is null or is not null")
	}
	p.fail(t, "expected a column, a literal or a function call, got %s", describe(t))
	return nil
}
