package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/candyflipline/mnemonic/arena/keeper"
	"github.com/candyflipline/mnemonic/arena/obj"
)

// Coverage says what an Answer covers.
type Coverage string

const (
	// CoverAll is the committed parts plus the keeper's hot parts for that
	// manifest.
	CoverAll Coverage = "all"
	// CoverCommitted is the committed manifest only: while a source is hot its
	// whole component is absent from the manifest, so sessions active within
	// the keeper's -quiet window can be missing entirely, not just their newest
	// events.
	CoverCommitted Coverage = "committed"
)

var (
	ErrStale    = errors.New("route: no manifest confirmed latest within DeleteSlack/2")
	ErrNoKeeper = errors.New("route: no keeper for the whole rung")
)

// KeeperHead is the keeper's GET /v1/head: the manifest it committed at its own
// gen, and the hot parts that complement exactly that manifest. The keeper
// answers 503 until its first commit at its gen.
type KeeperHead struct {
	Gen keeper.Gen
	Seq keeper.Seq
	Hot []keeper.PartRef
}

// Keeper is the router's link to the writer: a Reader with ID LocalID that
// answers the keeper's hot parts and the Whole rung, plus its head.
type Keeper interface {
	Reader
	Head(ctx context.Context) (KeeperHead, error)
}

// HTTPKeeper is the Keeper at baseURL; Head fails within headTimeout, Call
// within callTimeout. A Call that got no answer is an InfraError naming
// LocalID; a 500 is the tool's own error text.
func HTTPKeeper(baseURL string, headTimeout, callTimeout time.Duration) Keeper {
	return &httpKeeper{Reader: HTTPReader(LocalID, baseURL, callTimeout), base: baseURL, head: &http.Client{Timeout: headTimeout}}
}

type httpKeeper struct {
	Reader
	base string
	head *http.Client
}

func (k *httpKeeper) Call(ctx context.Context, req CallRequest) (CallResponse, error) {
	resp, err := k.Reader.Call(ctx, req)
	var se statusError
	if err != nil && !(errors.As(err, &se) && se.code == http.StatusInternalServerError) {
		return resp, InfraError{error: err, Reader: LocalID}
	}
	return resp, err
}

func (k *httpKeeper) Head(ctx context.Context) (KeeperHead, error) {
	var h KeeperHead
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.base+"/v1/head", nil)
	if err != nil {
		return h, err
	}
	res, err := k.head.Do(req)
	if err != nil {
		return h, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return h, fmt.Errorf("keeper head: %s", res.Status)
	}
	return h, json.NewDecoder(res.Body).Decode(&h)
}

type Answer struct {
	Data     json.RawMessage
	Text     string
	Gen      keeper.Gen
	Seq      keeper.Seq
	Coverage Coverage
}

// Cursor is a reader's place in the view stream: the manifest it holds and
// when its grants expire. Its wire form is %016x-%016x-<unix ms>, with 0 for
// no expiry.
type Cursor struct {
	Gen     keeper.Gen
	Seq     keeper.Seq
	Expires time.Time
}

func (c Cursor) String() string {
	var ms int64
	if !c.Expires.IsZero() {
		ms = c.Expires.UnixMilli()
	}
	return fmt.Sprintf("%016x-%016x-%d", uint64(c.Gen), uint64(c.Seq), ms)
}

func ParseCursor(s string) (Cursor, error) {
	bad := fmt.Errorf("route: cursor %q is not %%016x-%%016x-<unix ms>", s)
	parts := strings.Split(s, "-")
	if len(parts) != 3 || len(parts[0]) != 16 || len(parts[1]) != 16 {
		return Cursor{}, bad
	}
	g, err1 := strconv.ParseUint(parts[0], 16, 64)
	q, err2 := strconv.ParseUint(parts[1], 16, 64)
	ms, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || ms < 0 {
		return Cursor{}, bad
	}
	c := Cursor{Gen: keeper.Gen(g), Seq: keeper.Seq(q)}
	if ms > 0 {
		c.Expires = time.UnixMilli(ms)
	}
	return c, nil
}

type Router struct {
	tenant  string
	s       obj.ReadStore
	k       Keeper
	readers []Reader
	listed  atomic.Pointer[listed]
	changed atomic.Pointer[chan struct{}]

	mu     sync.Mutex
	loaded map[manifestID]keeper.Manifest
}

type manifestID struct {
	gen keeper.Gen
	seq keeper.Seq
}

// listed is the highest (gen, seq) a List showed, and when that List started.
type listed struct {
	m  keeper.Manifest
	at time.Time
}

const (
	listEvery      = 2 * time.Second
	viewWait       = 30 * time.Second
	loadedManifest = 4
)

// NewRouter routes tenant's calls over s's manifests; k may be nil.
func NewRouter(tenant string, s obj.ReadStore, k Keeper, readers []Reader) *Router {
	r := &Router{tenant: tenant, s: s, k: k, readers: readers, loaded: map[manifestID]keeper.Manifest{}}
	ch := make(chan struct{})
	r.changed.Store(&ch)
	return r
}

// Run lists m/ every min(2s, DeleteSlack/6) until ctx ends; a List that fails
// leaves listed alone, so its age decides staleness.
func (r *Router) Run(ctx context.Context) error {
	for {
		r.refresh(ctx)
		every := listEvery
		if l := r.listed.Load(); l != nil {
			every = min(every, l.m.Fence.DeleteSlack/6)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}

func above(g keeper.Gen, q keeper.Seq, og keeper.Gen, oq keeper.Seq) bool {
	return g > og || (g == og && q > oq)
}

func (r *Router) refresh(ctx context.Context) {
	at := time.Now()
	g, q, ok, err := keeper.Newest(ctx, r.s, r.tenant)
	if err != nil || !ok {
		return
	}
	prev := r.listed.Load()
	switch {
	case prev == nil || above(g, q, prev.m.Gen, prev.m.Seq):
		m, err := r.manifest(ctx, g, q)
		if err != nil {
			return
		}
		r.listed.Store(&listed{m: m, at: at})
		next := make(chan struct{})
		if old := r.changed.Swap(&next); old != nil {
			close(*old)
		}
	case g == prev.m.Gen && q == prev.m.Seq:
		r.listed.Store(&listed{m: prev.m, at: at})
	}
}

// fresh returns the listed manifest if its confirming List started within
// DeleteSlack/2, with the absolute expiry every grant made from it carries.
func (r *Router) fresh() (l *listed, notAfter time.Time, err error) {
	l = r.listed.Load()
	if l == nil || time.Since(l.at) > l.m.Fence.DeleteSlack/2 {
		return nil, time.Time{}, ErrStale
	}
	return l, l.at.Add(l.m.Fence.DeleteSlack/2 + l.m.Fence.GrantTTL), nil
}

// manifest loads m/<g>-<q>.json, once per key.
func (r *Router) manifest(ctx context.Context, g keeper.Gen, q keeper.Seq) (keeper.Manifest, error) {
	id := manifestID{g, q}
	r.mu.Lock()
	m, ok := r.loaded[id]
	r.mu.Unlock()
	if ok {
		return m, nil
	}
	m, err := keeper.Load(ctx, r.s, r.tenant, g, q)
	if err != nil {
		return m, err
	}
	r.mu.Lock()
	if len(r.loaded) >= loadedManifest {
		clear(r.loaded)
	}
	r.loaded[id] = m
	r.mu.Unlock()
	return m, nil
}

// pair chooses the manifest a call runs over and the keeper's hot parts that
// complement it: the keeper's own head when it is at or above the listed one,
// the listed manifest alone otherwise.
func (r *Router) pair(ctx context.Context, l *listed) (keeper.Manifest, []keeper.PartRef, Coverage) {
	if r.k == nil {
		return l.m, nil, CoverCommitted
	}
	h, err := r.k.Head(ctx)
	switch {
	case err != nil:
	case h.Gen == l.m.Gen && h.Seq == l.m.Seq:
		return l.m, h.Hot, CoverAll
	case above(h.Gen, h.Seq, l.m.Gen, l.m.Seq):
		if m, err := r.manifest(ctx, h.Gen, h.Seq); err == nil {
			return m, h.Hot, CoverAll
		}
	}
	return l.m, nil, CoverCommitted
}

func (r *Router) grant(m keeper.Manifest, hot []keeper.PartRef, notAfter time.Time) (View, error) {
	v := View{Tenant: m.Tenant, Gen: m.Gen, Seq: m.Seq, Sources: m.Sources, Expires: notAfter, Parts: make([]GrantedPart, 0, len(m.Parts)+len(hot))}
	for _, p := range m.Parts {
		u, err := r.s.PresignGetUntil(p.Key, notAfter)
		if err != nil {
			return v, err
		}
		v.Parts = append(v.Parts, GrantedPart{PartRef: p, URL: u})
	}
	for _, p := range hot {
		v.Parts = append(v.Parts, GrantedPart{PartRef: p, Local: true})
	}
	return v, nil
}

// Call answers one tool call over the latest listed manifest, paired with the
// keeper's hot parts when the keeper's head matches it.
//  1. Refuse with ErrStale once the List that last showed the manifest started
//     more than DeleteSlack/2 ago; the call runs under a deadline of MaxCall.
//  2. Ask the keeper for its head: above the listed manifest, its manifest and
//     hot parts; equal, the listed manifest and its hot parts; otherwise, or on
//     an error, the listed manifest alone and coverage committed.
//  3. Presign every cold part until the absolute expiry listed.at +
//     DeleteSlack/2 + GrantTTL; hot parts are Local with no URL.
//  4. Do the call over the remote readers plus the keeper when paired. An
//     InfraError naming the keeper reruns a Partial call once without hot parts.
func (r *Router) Call(ctx context.Context, rung Rung, tool string, args json.RawMessage) (Answer, error) {
	l, notAfter, err := r.fresh()
	if err != nil {
		return Answer{}, err
	}
	if rung == Whole && r.k == nil && !viewOnly(tool) {
		return Answer{}, ErrNoKeeper
	}
	if mc := l.m.Fence.MaxCall; mc > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, mc)
		defer cancel()
	}
	m, hot, cover := r.pair(ctx, l)
	if rung == Whole && cover != CoverAll && !viewOnly(tool) {
		return Answer{}, ErrNoKeeper
	}
	run := func(hot []keeper.PartRef, cover Coverage) (Answer, error) {
		ans := Answer{Gen: m.Gen, Seq: m.Seq, Coverage: cover}
		v, err := r.grant(m, hot, notAfter)
		if err != nil {
			return ans, InfraError{error: err}
		}
		readers := r.readers
		if cover == CoverAll {
			readers = append(append([]Reader(nil), readers...), r.k)
		}
		ans.Data, ans.Text, err = Do(ctx, v, readers, rung, tool, args)
		return ans, err
	}
	ans, err := run(hot, cover)
	var infra InfraError
	if rung == Partial && cover == CoverAll && errors.As(err, &infra) && infra.Reader == LocalID && ctx.Err() == nil {
		return run(nil, CoverCommitted)
	}
	return ans, err
}

// View is GET /v1/view?after=: the served manifest's cold parts freshly
// granted, once the manifest is above after's or after's grants are within a
// third of GrantTTL of expiry, waiting up to 30 s; ok is false when neither
// happened.
func (r *Router) View(ctx context.Context, after Cursor) (v View, ok bool, err error) {
	wait := time.NewTimer(viewWait)
	defer wait.Stop()
	for waited := false; ; waited = true {
		ch := *r.changed.Load()
		if l := r.listed.Load(); l != nil && viewDue(l.m, after) {
			v, err := r.coldView()
			return v, err == nil, err
		}
		if waited {
			return View{}, false, nil
		}
		select {
		case <-ch:
		case <-wait.C:
		case <-ctx.Done():
			return View{}, false, ctx.Err()
		}
	}
}

func viewDue(m keeper.Manifest, after Cursor) bool {
	return after == (Cursor{}) ||
		above(m.Gen, m.Seq, after.Gen, after.Seq) ||
		(!after.Expires.IsZero() && time.Until(after.Expires) < m.Fence.GrantTTL/3)
}

func (r *Router) coldView() (View, error) {
	l, notAfter, err := r.fresh()
	if err != nil {
		return View{}, err
	}
	return r.grant(l.m, nil, notAfter)
}
