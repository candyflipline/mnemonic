package ql

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
)

type RowSource interface {
	Scan(ctx context.Context, req ScanRequest, fn func(Row) error) error
}

type ScanRequest struct {
	Relation string
	Columns  []Column
	Match    []MatchHint
}

type MatchHint struct {
	Column string
	Words  []string
}

type Row interface {
	Get(i int) (Value, error)
}

type Limits struct {
	Rows int
	Held int
}

type ResultColumn struct {
	Name string `json:"name"`
	Type Type   `json:"type"`
}

type Result struct {
	Columns   []ResultColumn
	Rows      [][]Value
	Truncated bool
}

type rowCtx struct {
	row  Row
	cols []Column
	vals []Value
	have []bool
}

func (r *rowCtx) reset(row Row) {
	r.row = row
	clear(r.have)
}

func (r *rowCtx) get(i int) (Value, error) {
	if !r.have[i] {
		v, err := r.row.Get(i)
		if err != nil {
			return Null(), err
		}
		if !v.IsNull() && v.t != r.cols[i].Type {
			return Null(), fmt.Errorf("ql: the source returned %s for column %s of type %s", v.t, r.cols[i].Name, r.cols[i].Type)
		}
		r.vals[i], r.have[i] = v, true
	}
	return r.vals[i], nil
}

var errStop = errors.New("ql: enough rows")

const ctxEvery = 256

func Run(ctx context.Context, p Plan, src RowSource, lim Limits) (Result, error) {
	if lim.Rows < 1 || lim.Held < 1 {
		return Result{}, fmt.Errorf("ql: limits must be positive, got %+v", lim)
	}
	want := lim.Rows + 1
	if p.limit > 0 && p.limit < want {
		want = p.limit
	}
	rc := &rowCtx{cols: p.req.Columns, vals: make([]Value, len(p.req.Columns)), have: make([]bool, len(p.req.Columns))}
	var seen int
	pass := func(row Row) (bool, error) {
		if seen++; seen%ctxEvery == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		rc.reset(row)
		for _, c := range p.conjuncts {
			v, err := c.eval(rc)
			if err != nil || !v.truth() {
				return false, err
			}
		}
		return true, nil
	}
	var rows [][]Value
	var err error
	if p.grouped {
		rows, err = runGroups(ctx, p, src, rc, pass, lim.Held)
		if err == nil {
			sortEntries(p, rows)
			rows = rows[:min(len(rows), want)]
		}
	} else {
		rows, err = runRows(ctx, p, src, rc, pass, want)
	}
	if err != nil {
		return Result{}, err
	}
	res := Result{Columns: p.out, Rows: rows}
	if len(rows) > lim.Rows {
		res.Rows, res.Truncated = rows[:lim.Rows], true
	}
	return res, nil
}

func scan(ctx context.Context, src RowSource, req ScanRequest, fn func(Row) error) error {
	err := src.Scan(ctx, req, fn)
	if errors.Is(err, errStop) {
		err = nil
	}
	if err == nil {
		err = ctx.Err()
	}
	return err
}

func evalAll(rc *rowCtx, es []bexpr) ([]Value, error) {
	out := make([]Value, len(es))
	for i, e := range es {
		v, err := e.eval(rc)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

type entry struct {
	out, keys []Value
	seq       int
}

func runRows(ctx context.Context, p Plan, src RowSource, rc *rowCtx, pass func(Row) (bool, error), want int) ([][]Value, error) {
	var top topK
	top.p, top.want = p, want
	var rows [][]Value
	err := scan(ctx, src, p.req, func(row Row) error {
		ok, err := pass(row)
		if !ok || err != nil {
			return err
		}
		out, err := evalAll(rc, p.sel)
		if err != nil {
			return err
		}
		if len(p.sort) == 0 {
			rows = append(rows, out)
			if len(rows) == want {
				return errStop
			}
			return nil
		}
		keys := make([]Value, len(p.sort))
		for i, s := range p.sort {
			if s.out >= 0 {
				keys[i] = out[s.out]
			} else if keys[i], err = s.e.eval(rc); err != nil {
				return err
			}
		}
		top.push(entry{out: out, keys: keys, seq: top.seq})
		top.seq++
		return nil
	})
	if err != nil || len(p.sort) == 0 {
		return rows, err
	}
	es := top.es
	slices.SortFunc(es, func(a, b entry) int { return cmpEntries(p, a, b) })
	rows = make([][]Value, len(es))
	for i, e := range es {
		rows[i] = e.out
	}
	return rows, nil
}

func cmpEntries(p Plan, a, b entry) int {
	for i, s := range p.sort {
		x, y := a.keys[i], b.keys[i]
		var c int
		switch {
		case x.IsNull() && y.IsNull():
		case x.IsNull():
			return 1
		case y.IsNull():
			return -1
		default:
			c = compare(x, y)
		}
		if s.desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return a.seq - b.seq
}

type topK struct {
	p    Plan
	want int
	seq  int
	es   []entry
}

func (t *topK) Len() int           { return len(t.es) }
func (t *topK) Less(i, j int) bool { return cmpEntries(t.p, t.es[i], t.es[j]) > 0 }
func (t *topK) Swap(i, j int)      { t.es[i], t.es[j] = t.es[j], t.es[i] }
func (t *topK) Push(x any)         { t.es = append(t.es, x.(entry)) }
func (t *topK) Pop() any {
	e := t.es[len(t.es)-1]
	t.es = t.es[:len(t.es)-1]
	return e
}

func (t *topK) push(e entry) {
	if len(t.es) < t.want {
		heap.Push(t, e)
		return
	}
	if cmpEntries(t.p, e, t.es[0]) < 0 {
		t.es[0] = e
		heap.Fix(t, 0)
	}
}

func sortEntries(p Plan, rows [][]Value) {
	if len(p.sort) == 0 {
		return
	}
	keyed := make([]entry, len(rows))
	for i, r := range rows {
		keys := make([]Value, len(p.sort))
		for j, s := range p.sort {
			keys[j] = r[s.out]
		}
		keyed[i] = entry{out: r, keys: keys, seq: i}
	}
	sort.SliceStable(keyed, func(i, j int) bool { return cmpEntries(p, keyed[i], keyed[j]) < 0 })
	for i, e := range keyed {
		rows[i] = e.out
	}
}

type group struct {
	dims []Value
	accs []acc
}

type budget struct {
	held, max int
}

func (b *budget) take() error {
	if b.held++; b.held > b.max {
		return &Error{Msg: fmt.Sprintf("the query holds more than %d groups and values in memory; add a filter or group by fewer dimensions", b.max)}
	}
	return nil
}

func runGroups(ctx context.Context, p Plan, src RowSource, rc *rowCtx, pass func(Row) (bool, error), held int) ([][]Value, error) {
	b := &budget{max: held}
	groups := map[string]*group{}
	var order []*group
	newGroup := func(dims []Value) *group {
		g := &group{dims: dims, accs: make([]acc, len(p.aggs))}
		for i, a := range p.aggs {
			g.accs[i] = newAcc(a, b)
		}
		return g
	}
	var key []byte
	err := scan(ctx, src, p.req, func(row Row) error {
		ok, err := pass(row)
		if !ok || err != nil {
			return err
		}
		dims, err := evalAll(rc, p.dims)
		if err != nil {
			return err
		}
		key = key[:0]
		for _, d := range dims {
			key = appendKey(key, d)
		}
		g := groups[string(key)]
		if g == nil {
			if err := b.take(); err != nil {
				return err
			}
			g = newGroup(dims)
			groups[string(key)] = g
			order = append(order, g)
		}
		for _, a := range g.accs {
			if err := a.add(rc); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(order) == 0 && len(p.dims) == 0 {
		order = append(order, newGroup(nil))
	}
	rows := make([][]Value, len(order))
	for i, g := range order {
		r := append([]Value(nil), g.dims...)
		for _, a := range g.accs {
			r = append(r, a.result())
		}
		rows[i] = r
	}
	return rows, nil
}

type acc interface {
	add(*rowCtx) error
	result() Value
}

func newAcc(a aggSpec, b *budget) acc {
	switch a.kind {
	case aggCount:
		return &countAcc{arg: a.arg}
	case aggCountDistinct:
		return &distinctAcc{arg: a.arg, seen: map[string]bool{}, b: b}
	case aggSum, aggAvg:
		return &sumAcc{arg: a.arg, t: a.t, avg: a.kind == aggAvg}
	case aggMin, aggMax:
		return &extremeAcc{arg: a.arg, max: a.kind == aggMax}
	}
	return &percentileAcc{arg: a.arg, p: a.p, b: b}
}

type countAcc struct {
	arg *bexpr
	n   int64
}

func (a *countAcc) add(rc *rowCtx) error {
	if a.arg != nil {
		v, err := a.arg.eval(rc)
		if err != nil || v.IsNull() {
			return err
		}
	}
	a.n++
	return nil
}

func (a *countAcc) result() Value { return IntValue(a.n) }

type distinctAcc struct {
	arg  *bexpr
	seen map[string]bool
	b    *budget
}

func (a *distinctAcc) add(rc *rowCtx) error {
	v, err := a.arg.eval(rc)
	if err != nil || v.IsNull() {
		return err
	}
	k := string(appendKey(nil, v))
	if a.seen[k] {
		return nil
	}
	a.seen[k] = true
	return a.b.take()
}

func (a *distinctAcc) result() Value { return IntValue(int64(len(a.seen))) }

type sumAcc struct {
	arg *bexpr
	t   Type
	avg bool
	n   int64
	i   int64
	f   float64
}

func (a *sumAcc) add(rc *rowCtx) error {
	v, err := a.arg.eval(rc)
	if err != nil || v.IsNull() {
		return err
	}
	a.n++
	a.i += v.n
	a.f += v.num()
	return nil
}

func (a *sumAcc) result() Value {
	switch {
	case a.n == 0:
		return Null()
	case a.avg:
		return FloatValue(a.f / float64(a.n))
	case a.t == TInt:
		return IntValue(a.i)
	}
	return FloatValue(a.f)
}

type extremeAcc struct {
	arg *bexpr
	max bool
	v   Value
}

func (a *extremeAcc) add(rc *rowCtx) error {
	v, err := a.arg.eval(rc)
	if err != nil || v.IsNull() {
		return err
	}
	if a.v.IsNull() || (compare(v, a.v) > 0) == a.max && compare(v, a.v) != 0 {
		a.v = v
	}
	return nil
}

func (a *extremeAcc) result() Value { return a.v }

type percentileAcc struct {
	arg *bexpr
	p   float64
	xs  []float64
	b   *budget
}

func (a *percentileAcc) add(rc *rowCtx) error {
	v, err := a.arg.eval(rc)
	if err != nil || v.IsNull() {
		return err
	}
	a.xs = append(a.xs, v.num())
	return a.b.take()
}

func (a *percentileAcc) result() Value {
	if len(a.xs) == 0 {
		return Null()
	}
	slices.Sort(a.xs)
	pos := a.p * float64(len(a.xs)-1)
	lo := math.Floor(pos)
	hi := math.Ceil(pos)
	x := a.xs[int(lo)] + (a.xs[int(hi)]-a.xs[int(lo)])*(pos-lo)
	return FloatValue(x)
}
