package ql

import (
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Plan struct {
	req       ScanRequest
	conjuncts []bexpr
	out       []ResultColumn
	sel       []bexpr
	grouped   bool
	dims      []bexpr
	aggs      []aggSpec
	sort      []sortSpec
	limit     int
}

func (p Plan) Columns() []ResultColumn { return p.out }

type bexpr struct {
	t     Type
	heavy bool
	eval  func(*rowCtx) (Value, error)
}

type sortSpec struct {
	out  int
	e    bexpr
	desc bool
}

type aggKind uint8

const (
	aggCount aggKind = iota + 1
	aggCountDistinct
	aggSum
	aggAvg
	aggMin
	aggMax
	aggPercentile
)

type aggSpec struct {
	kind aggKind
	arg  *bexpr
	p    float64
	t    Type
}

var aggNames = map[string]aggKind{
	"count": aggCount, "count_distinct": aggCountDistinct, "sum": aggSum, "avg": aggAvg,
	"min": aggMin, "max": aggMax, "percentile": aggPercentile,
}

type scalarFunc struct {
	arg, ret Type
	f        func(Value) Value
}

var scalarFuncs = map[string]scalarFunc{
	"lower":  {TString, TString, func(v Value) Value { return StringValue(strings.ToLower(v.s)) }},
	"length": {TString, TInt, func(v Value) Value { return IntValue(int64(utf8.RuneCountInString(v.s))) }},
	"hour":   {TTime, TTime, func(v Value) Value { return TimeValue(v.time().Truncate(time.Hour)) }},
	"day":    {TTime, TTime, func(v Value) Value { return TimeValue(v.time().Truncate(24 * time.Hour)) }},
	"week": {TTime, TTime, func(v Value) Value {
		d := v.time().Truncate(24 * time.Hour)
		return TimeValue(d.AddDate(0, 0, -(int(d.Weekday())+6)%7))
	}},
	"month": {TTime, TTime, func(v Value) Value {
		t := v.time()
		return TimeValue(time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC))
	}},
}

const functionList = "lower, length, hour, day, week, month, and in measures: count, count_distinct, sum, avg, min, max, percentile"

type checker struct {
	rel  *Relation
	scan map[int]int
	req  *ScanRequest
}

func Check(q Query, s Schema) (p Plan, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*Error)
			if !ok {
				panic(r)
			}
			p, err = Plan{}, e
		}
	}()
	rel, ok := s.relation(q.From.Name)
	if !ok {
		panic(errorf(q.From.At, "relation %q is not queryable; query one of %s", q.From.Name, strings.Join(s.names(), ", ")))
	}
	p.req.Relation = rel.Name
	c := &checker{rel: rel, scan: map[int]int{}, req: &p.req}
	p.limit = q.Limit
	switch sh := q.Shape.(type) {
	case SelectAll:
		for i, col := range rel.Columns {
			if !col.Heavy {
				p.sel = append(p.sel, c.column(i))
				p.out = append(p.out, ResultColumn{Name: col.Name, Type: col.Type})
			}
		}
	case Select:
		for _, it := range sh.Items {
			e := c.bind(it.Expr)
			p.sel = append(p.sel, e)
			p.out = append(p.out, ResultColumn{Name: it.Name, Type: e.t})
		}
		c.unique(p.out, sh.Items)
	case Group:
		p.grouped = true
		var items []Item
		for _, it := range sh.Dimensions {
			e := c.bind(it.Expr)
			if !e.t.scalar() {
				panic(errorf(it.Expr.Pos(), "cannot group by %s", e.t))
			}
			p.dims = append(p.dims, e)
			p.out = append(p.out, ResultColumn{Name: it.Name, Type: e.t})
			items = append(items, it)
		}
		for _, it := range sh.Measures {
			a := c.measure(it)
			p.aggs = append(p.aggs, a)
			p.out = append(p.out, ResultColumn{Name: it.Name, Type: a.t})
			items = append(items, it)
		}
		c.unique(p.out, items)
	}
	if q.Filter != nil {
		var cheap, heavy []bexpr
		for _, e := range conjuncts(q.Filter) {
			b := c.bind(e)
			if b.t != TBool {
				panic(errorf(e.Pos(), "filter must be true or false, got %s", b.t))
			}
			if m, ok := e.(*Match); ok {
				p.req.Match = append(p.req.Match, MatchHint{Column: m.Col.Name, Words: words(m.Text)})
			}
			if b.heavy {
				heavy = append(heavy, b)
			} else {
				cheap = append(cheap, b)
			}
		}
		p.conjuncts = append(cheap, heavy...)
	}
	for _, k := range q.Sort {
		p.sort = append(p.sort, c.sortKey(k, p.out, p.grouped))
	}
	return p, nil
}

func conjuncts(e Expr) []Expr {
	if b, ok := e.(*Binary); ok && b.Op == OpAnd {
		return append(conjuncts(b.L), conjuncts(b.R)...)
	}
	return []Expr{e}
}

func (c *checker) unique(out []ResultColumn, items []Item) {
	for i := range out {
		for j := range i {
			if out[i].Name == out[j].Name {
				panic(errorf(items[i].Expr.Pos(), "two output columns are named %q; rename one with as", out[i].Name))
			}
		}
	}
}

func (c *checker) column(i int) bexpr {
	si, ok := c.scan[i]
	if !ok {
		si = len(c.req.Columns)
		c.scan[i] = si
		c.req.Columns = append(c.req.Columns, c.rel.Columns[i])
	}
	col := c.rel.Columns[i]
	return bexpr{t: col.Type, heavy: col.Heavy, eval: func(r *rowCtx) (Value, error) { return r.get(si) }}
}

func (c *checker) lookup(col *Col) (int, Column) {
	i, ok := c.rel.column(col.Name)
	if !ok {
		panic(errorf(col.At, "%s has no column %q", c.rel.Name, col.Name))
	}
	return i, c.rel.Columns[i]
}

func constant(v Value) bexpr {
	return bexpr{t: v.t, eval: func(*rowCtx) (Value, error) { return v, nil }}
}

var timeLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02T15:04:05.999999", "2006-01-02"}

func (c *checker) operand(e Expr, want Type) bexpr {
	if lit, ok := e.(*Lit); ok && want == TTime && lit.Val.t == TString {
		return constant(TimeValue(parseTime(lit)))
	}
	return c.bind(e)
}

func parseTime(lit *Lit) time.Time {
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, lit.Val.s); err == nil {
			return t.UTC()
		}
	}
	panic(errorf(lit.At, "%q is not a time; write RFC 3339, for example '2026-02-15T03:50:41Z'", lit.Val.s))
}

func comparable(a, b Type) bool {
	return a == b && a.scalar() || a.numeric() && b.numeric()
}

func (c *checker) bind(e Expr) bexpr {
	switch e := e.(type) {
	case *Col:
		i, _ := c.lookup(e)
		return c.column(i)
	case *Lit:
		return constant(e.Val)
	case *Unary:
		x := c.bind(e.X)
		if e.Op == OpNot {
			if x.t != TBool {
				panic(errorf(e.At, "not takes true or false, got %s", x.t))
			}
			return lift1(TBool, x, func(v Value) Value { return BoolValue(!v.truth()) })
		}
		if !x.t.numeric() {
			panic(errorf(e.At, "- takes a number, got %s", x.t))
		}
		return lift1(x.t, x, func(v Value) Value {
			if v.t == TInt {
				return IntValue(-v.n)
			}
			return FloatValue(-v.f)
		})
	case *Binary:
		return c.binary(e)
	case *In:
		x := c.bind(e.X)
		if !x.t.scalar() {
			panic(errorf(e.At, "in takes a single value on its left, got %s", x.t))
		}
		set := map[string]bool{}
		for _, lit := range e.List {
			v := c.operand(&lit, x.t)
			if !comparable(x.t, v.t) {
				panic(errorf(lit.At, "in (...) lists %s, but the value on its left is %s", v.t, x.t))
			}
			k, _ := v.eval(nil)
			set[string(appendKey(nil, k))] = true
		}
		return lift1(TBool, x, func(v Value) Value { return BoolValue(set[string(appendKey(nil, v))] != e.Not) })
	case *IsNull:
		x := c.bind(e.X)
		return bexpr{t: TBool, heavy: x.heavy, eval: func(r *rowCtx) (Value, error) {
			v, err := x.eval(r)
			return BoolValue(v.IsNull() != e.Not), err
		}}
	case *Like:
		x := c.bind(e.X)
		if x.t != TString {
			panic(errorf(e.At, "like takes a string on its left, got %s", x.t))
		}
		pat := e.Pattern
		if e.Fold {
			pat = strings.ToLower(pat)
		}
		if len(e.Pattern) > likeMaxPattern {
			panic(errorf(e.At, "like pattern must be at most %d bytes", likeMaxPattern))
		}
		lm := compileLike(pat)
		return lift1(TBool, x, func(v Value) Value {
			s := v.s
			if e.Fold {
				s = strings.ToLower(s)
			}
			return BoolValue(lm.match(s) != e.Not)
		})
	case *Match:
		i, col := c.lookup(&e.Col)
		if col.Type != TString {
			panic(errorf(e.At, "match takes a string column; %s is %s", col.Name, col.Type))
		}
		want := words(e.Text)
		if len(want) == 0 {
			panic(errorf(e.At, "match needs at least one word"))
		}
		return lift1(TBool, c.column(i), func(v Value) Value { return BoolValue(hasWords(v.s, want)) })
	case *Includes:
		x := c.bind(e.X)
		if x.t != TStrings {
			panic(errorf(e.At, "includes takes a list<string> on its left, got %s", x.t))
		}
		return lift1(TBool, x, func(v Value) Value { return BoolValue(slices.Contains(v.x.([]string), e.Elem)) })
	case *Index:
		i, col := c.lookup(&e.Col)
		switch col.Type {
		case TIntMap:
			return lift1(TInt, c.column(i), func(v Value) Value {
				if n, ok := v.x.(map[string]int64)[e.Key]; ok {
					return IntValue(n)
				}
				return Null()
			})
		case TFloatMap:
			return lift1(TFloat, c.column(i), func(v Value) Value {
				if f, ok := v.x.(map[string]float64)[e.Key]; ok {
					return FloatValue(f)
				}
				return Null()
			})
		}
		panic(errorf(e.At, "%s is %s; only a map column takes ['key']", col.Name, col.Type))
	case *Call:
		if _, ok := aggNames[e.Name]; ok {
			panic(errorf(e.At, "%s is an aggregate; it belongs in measures:", e.Name))
		}
		f, ok := scalarFuncs[e.Name]
		if !ok {
			panic(errorf(e.At, "function %q is not allowed; the functions are %s", e.Name, functionList))
		}
		if e.Star || len(e.Args) != 1 {
			panic(errorf(e.At, "%s takes one %s argument", e.Name, f.arg))
		}
		x := c.bind(e.Args[0])
		if x.t != f.arg {
			panic(errorf(e.At, "%s takes %s, got %s", e.Name, f.arg, x.t))
		}
		return lift1(f.ret, x, f.f)
	}
	panic(errorf(e.Pos(), "unsupported expression"))
}

func lift1(t Type, x bexpr, f func(Value) Value) bexpr {
	return bexpr{t: t, heavy: x.heavy, eval: func(r *rowCtx) (Value, error) {
		v, err := x.eval(r)
		if err != nil || v.IsNull() {
			return Null(), err
		}
		return f(v), nil
	}}
}

func lift2(t Type, l, r bexpr, f func(a, b Value) Value) bexpr {
	return bexpr{t: t, heavy: l.heavy || r.heavy, eval: func(rc *rowCtx) (Value, error) {
		a, err := l.eval(rc)
		if err != nil || a.IsNull() {
			return Null(), err
		}
		b, err := r.eval(rc)
		if err != nil || b.IsNull() {
			return Null(), err
		}
		return f(a, b), nil
	}}
}

func (c *checker) binary(e *Binary) bexpr {
	switch e.Op {
	case OpAnd, OpOr:
		l, r := c.bind(e.L), c.bind(e.R)
		if l.t != TBool || r.t != TBool {
			panic(errorf(e.At, "%s takes true or false on both sides, got %s and %s", e.Op, l.t, r.t))
		}
		decide := e.Op == OpOr
		return bexpr{t: TBool, heavy: l.heavy || r.heavy, eval: func(rc *rowCtx) (Value, error) {
			a, err := l.eval(rc)
			if err != nil || !a.IsNull() && a.truth() == decide {
				return a, err
			}
			b, err := r.eval(rc)
			if err != nil || !b.IsNull() && b.truth() == decide {
				return b, err
			}
			if a.IsNull() || b.IsNull() {
				return Null(), nil
			}
			return BoolValue(!decide), nil
		}}
	case OpAdd, OpSub, OpMul, OpDiv:
		l, r := c.bind(e.L), c.bind(e.R)
		if !l.t.numeric() || !r.t.numeric() {
			panic(errorf(e.At, "%s takes numbers, got %s and %s", e.Op, l.t, r.t))
		}
		t := TFloat
		if l.t == TInt && r.t == TInt && e.Op != OpDiv {
			t = TInt
		}
		op := e.Op
		return lift2(t, l, r, func(a, b Value) Value { return arith(op, t, a, b) })
	}
	var l, r bexpr
	switch {
	case isStringLit(e.L):
		r = c.bind(e.R)
		l = c.operand(e.L, r.t)
	default:
		l = c.bind(e.L)
		r = c.operand(e.R, l.t)
	}
	if !comparable(l.t, r.t) {
		panic(errorf(e.At, "cannot compare %s with %s", l.t, r.t))
	}
	op := e.Op
	return lift2(TBool, l, r, func(a, b Value) Value {
		n := compare(a, b)
		switch op {
		case OpEq:
			return BoolValue(n == 0)
		case OpNe:
			return BoolValue(n != 0)
		case OpLt:
			return BoolValue(n < 0)
		case OpLe:
			return BoolValue(n <= 0)
		case OpGt:
			return BoolValue(n > 0)
		}
		return BoolValue(n >= 0)
	})
}

func isStringLit(e Expr) bool {
	l, ok := e.(*Lit)
	return ok && l.Val.t == TString
}

func arith(op Op, t Type, a, b Value) Value {
	if t == TInt {
		switch op {
		case OpAdd:
			return IntValue(a.n + b.n)
		case OpSub:
			return IntValue(a.n - b.n)
		}
		return IntValue(a.n * b.n)
	}
	x, y := a.num(), b.num()
	switch op {
	case OpAdd:
		return FloatValue(x + y)
	case OpSub:
		return FloatValue(x - y)
	case OpMul:
		return FloatValue(x * y)
	}
	if y == 0 {
		return Null()
	}
	return FloatValue(x / y)
}

func (c *checker) measure(it Item) aggSpec {
	call, ok := it.Expr.(*Call)
	kind := aggNames[nameOf(call)]
	if !ok || kind == 0 {
		panic(errorf(it.Expr.Pos(), "measures takes aggregates (count, count_distinct, sum, avg, min, max, percentile); put %s in dimensions", it.Name))
	}
	if call.Star && kind != aggCount {
		panic(errorf(call.At, "only count takes *"))
	}
	if kind == aggCount && (call.Star || len(call.Args) == 0) {
		return aggSpec{kind: aggCount, t: TInt}
	}
	arity := 1
	if kind == aggPercentile {
		arity = 2
	}
	if len(call.Args) != arity {
		panic(errorf(call.At, "%s takes %d argument(s), got %d", call.Name, arity, len(call.Args)))
	}
	x := c.bind(call.Args[0])
	a := aggSpec{kind: kind, arg: &x, t: x.t}
	switch kind {
	case aggCount, aggCountDistinct:
		if kind == aggCountDistinct && !x.t.scalar() {
			panic(errorf(call.At, "count_distinct takes a single value, got %s", x.t))
		}
		a.t = TInt
	case aggSum, aggAvg, aggPercentile:
		if !x.t.numeric() {
			panic(errorf(call.At, "%s takes a number, got %s", call.Name, x.t))
		}
		if kind != aggSum {
			a.t = TFloat
		}
	case aggMin, aggMax:
		if !x.t.scalar() {
			panic(errorf(call.At, "%s takes a single value, got %s", call.Name, x.t))
		}
	}
	if kind == aggPercentile {
		lit, ok := call.Args[1].(*Lit)
		if !ok || !lit.Val.t.numeric() || lit.Val.num() < 0 || lit.Val.num() > 1 {
			panic(errorf(call.Args[1].Pos(), "percentile takes a fraction between 0 and 1 as its second argument, for example percentile(latency_ms, 0.95)"))
		}
		a.p = lit.Val.num()
	}
	return a
}

func nameOf(c *Call) string {
	if c == nil {
		return ""
	}
	return c.Name
}

func (c *checker) sortKey(k SortKey, out []ResultColumn, grouped bool) sortSpec {
	if col, ok := k.Expr.(*Col); ok {
		if i := slices.IndexFunc(out, func(o ResultColumn) bool { return o.Name == col.Name }); i >= 0 {
			if !out[i].Type.scalar() {
				panic(errorf(col.At, "cannot sort by %s", out[i].Type))
			}
			return sortSpec{out: i, desc: k.Desc}
		}
	}
	if grouped {
		names := make([]string, len(out))
		for i, o := range out {
			names[i] = o.Name
		}
		panic(errorf(k.Expr.Pos(), "a grouped query sorts by its output columns: %s", strings.Join(names, ", ")))
	}
	e := c.bind(k.Expr)
	if !e.t.scalar() {
		panic(errorf(k.Expr.Pos(), "cannot sort by %s", e.t))
	}
	return sortSpec{out: -1, e: e, desc: k.Desc}
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func hasWords(s string, want []string) bool {
	have := words(s)
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// likeMaxPattern caps a like/ilike pattern; the matcher below is linear in
// the string for a pattern this long.
const likeMaxPattern = 1000

// likeMatcher is a Shift-And automaton over a like pattern: state j is "j
// pattern characters matched", loop marks the states a % keeps alive. One
// pass over the string, no backtracking, at most 16 words of state.
type likeMatcher struct {
	n     int
	words int
	ascii [128][]uint64
	other map[rune][]uint64
	wild  []uint64
	loop  []uint64
}

func compileLike(pat string) *likeMatcher {
	var toks []rune
	loops := map[int]bool{}
	for _, r := range pat {
		if r == '%' {
			loops[len(toks)] = true
			continue
		}
		toks = append(toks, r)
	}
	n := len(toks)
	m := &likeMatcher{n: n, words: n/64 + 1, other: map[rune][]uint64{}}
	m.wild = make([]uint64, m.words)
	m.loop = make([]uint64, m.words)
	for j := range loops {
		m.loop[j/64] |= 1 << (j % 64)
	}
	for j, r := range toks {
		bit := j + 1
		if r == '_' {
			m.wild[bit/64] |= 1 << (bit % 64)
			continue
		}
		mask := m.mask(r)
		if mask == nil {
			mask = make([]uint64, m.words)
			if r < 128 {
				m.ascii[r] = mask
			} else {
				m.other[r] = mask
			}
		}
		mask[bit/64] |= 1 << (bit % 64)
	}
	return m
}

func (m *likeMatcher) mask(r rune) []uint64 {
	if r >= 0 && r < 128 {
		return m.ascii[r]
	}
	return m.other[r]
}

func (m *likeMatcher) match(s string) bool {
	var buf [17]uint64
	d := buf[:m.words]
	d[0] = 1
	acc, accBit := m.n/64, uint64(1)<<(m.n%64)
	accLoops := m.loop[acc]&accBit != 0
	for _, r := range s {
		mk := m.mask(r)
		var carry uint64
		alive := false
		for w := 0; w < m.words; w++ {
			shifted := d[w]<<1 | carry
			carry = d[w] >> 63
			t := m.wild[w]
			if mk != nil {
				t |= mk[w]
			}
			d[w] = shifted&t | d[w]&m.loop[w]
			if d[w] != 0 {
				alive = true
			}
		}
		if !alive {
			return false
		}
		if accLoops && d[acc]&accBit != 0 {
			return true
		}
	}
	return d[acc]&accBit != 0
}
