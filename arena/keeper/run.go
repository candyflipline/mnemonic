package keeper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/candyflipline/mnemonic/arena/obj"
	"github.com/candyflipline/mnemonic/arena/trace"
)

var ErrLost = errors.New("keeper: a newer keeper claimed the tenant")

var ErrUnchecked = errors.New("keeper: the gen has not been checked recently; refusing to grant")

var ErrDeleteSlack = errors.New("keeper: DeleteSlack is below the 3s floor")

// rescanEvery is how often sweep lists m/ and p/ again for older-gen objects, so
// a part a stale keeper uploads after a scan is doomed within one interval.
const rescanEvery = 10 * time.Minute

// MinDeleteSlack is the least DeleteSlack for which 2*checkEvery stays below it
// with checkEvery at its 1s floor.
const MinDeleteSlack = 3 * time.Second

type Config struct {
	Tenant      string
	Root        string
	Local       string
	Quiet       time.Duration
	Settle      bool
	GrantTTL    time.Duration
	DeleteSlack time.Duration
	MaxCall     time.Duration
}

func (c Config) fence() Fence {
	return Fence{GrantTTL: c.GrantTTL, DeleteSlack: c.DeleteSlack, MaxCall: c.MaxCall}
}

func maxFence(a, b Fence) Fence {
	return Fence{GrantTTL: max(a.GrantTTL, b.GrantTTL), DeleteSlack: max(a.DeleteSlack, b.DeleteSlack), MaxCall: max(a.MaxCall, b.MaxCall)}
}

func (f Fence) horizon() time.Duration { return f.GrantTTL + f.DeleteSlack + f.MaxCall }

// checkEvery keeps 2*checkEvery, the longest Read serves after the last good
// check, below DeleteSlack, the margin a successor's deletes leave past the
// last grant; Take refuses a DeleteSlack under MinDeleteSlack.
func (c Config) checkEvery() time.Duration {
	return min(c.DeleteSlack/3, 30*time.Second)
}

// Head is what the keeper serves: its committed manifest, the hot parts that
// stay on its disk, and every local part open by ID.
type Head struct {
	Manifest Manifest
	Hot      []PartRef
	Parts    map[string]*trace.Part
	epoch    uint64
}

type Keeper struct {
	cfg     Config
	s       obj.Store
	gen     Gen
	fence   Fence
	taken   Manifest
	lost    atomic.Bool
	checked atomic.Pointer[time.Time]

	cycle     sync.Mutex
	uploaded  map[string]PartRef
	doomed    map[obj.Key]time.Time
	scannedAt time.Time
	takenAt   time.Time

	mu      sync.Mutex
	head    atomic.Pointer[Head]
	stamp   uint64
	changed chan struct{}
	pins    map[uint64]int
	limbo   []dropped
	totals  trace.IngestStats

	closePart func(*trace.Part)
}

// dropped is a part an install left out of the head it made; heads up to epoch
// may still serve it.
type dropped struct {
	p     *trace.Part
	epoch uint64
}

type local struct {
	parts   []trace.LocalPart
	sources json.RawMessage
	open    map[string]*trace.Part
	stamp   uint64
}

// Take checks that s enforces exclusive create, claims the next gen, then
// reads the latest manifest; the first commit is the first Cycle. Cycle
// restores that manifest's parts to cfg.Local before it, so a keeper on an
// empty disk resumes where the store says the last keeper stopped.
func Take(ctx context.Context, s obj.Store, cfg Config) (*Keeper, error) {
	if cfg.DeleteSlack < MinDeleteSlack {
		return nil, fmt.Errorf("%w: got %s, want at least %s (raise -delete-slack)", ErrDeleteSlack, cfg.DeleteSlack, MinDeleteSlack)
	}
	if err := probeExclusive(ctx, s, cfg.Tenant); err != nil {
		return nil, err
	}
	claimed := time.Now()
	g, err := Claim(ctx, s, cfg.Tenant)
	if err != nil {
		return nil, err
	}
	m, _, err := Latest(ctx, s, cfg.Tenant)
	if err != nil {
		return nil, err
	}
	k := &Keeper{
		cfg:       cfg,
		s:         s,
		gen:       g,
		fence:     maxFence(cfg.fence(), m.Fence),
		taken:     m,
		uploaded:  map[string]PartRef{},
		doomed:    map[obj.Key]time.Time{},
		changed:   make(chan struct{}),
		pins:      map[uint64]int{},
		closePart: (*trace.Part).Close,
	}
	k.head.Store(&Head{Manifest: m})
	k.checked.Store(&claimed)
	return k, nil
}

func (k *Keeper) Gen() Gen { return k.gen }

// Totals is the sum of every Cycle's IngestStats since Take.
func (k *Keeper) Totals() trace.IngestStats {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.totals
}

// check fails with ErrLost once a higher gen is claimed, and keeps failing.
func (k *Keeper) check(ctx context.Context) (Gen, error) {
	start := time.Now()
	top, err := highestGen(ctx, k.s, k.cfg.Tenant)
	if err != nil && !k.lost.Load() {
		return 0, err
	}
	if k.lost.Load() || top != k.gen {
		k.lost.Store(true)
		return top, ErrLost
	}
	k.checked.Store(&start)
	return top, nil
}

// Run checks the gen once, then cycles every interval and checks it every
// checkEvery until ctx ends or the gen is lost.
func (k *Keeper) Run(ctx context.Context, every time.Duration) error {
	if _, err := k.check(ctx); errors.Is(err, ErrLost) {
		return err
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "keep: check:", err)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		t := time.NewTicker(k.cfg.checkEvery())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := k.check(ctx); errors.Is(err, ErrLost) {
					cancel(err)
					return
				} else if err != nil {
					fmt.Fprintln(os.Stderr, "keep: check:", err)
				}
			}
		}
	}()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-t.C:
		}
		err := k.Cycle(ctx)
		if errors.Is(err, ErrLost) {
			return err
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "keep: cycle:", err)
		}
	}
}

// Cycle ingests raw into the local store, commits its cold parts when they
// or the sources changed, and runs the deletes that are due.
func (k *Keeper) Cycle(ctx context.Context) error {
	if k.lost.Load() {
		return ErrLost
	}
	k.cycle.Lock()
	defer k.cycle.Unlock()
	if k.head.Load().Manifest.Gen != k.gen {
		if err := k.restore(ctx, k.taken); err != nil {
			return err
		}
	}
	st, err := trace.Ingest(k.cfg.Root, k.cfg.Local, k.cfg.Quiet, k.cfg.Settle)
	k.mu.Lock()
	for i := range st {
		k.totals[i] += st[i]
	}
	k.mu.Unlock()
	if err != nil {
		return err
	}
	stamp, ok := trace.CurrentStamp(k.cfg.Local)
	if !ok {
		return fmt.Errorf("keeper: %s has no CURRENT", k.cfg.Local)
	}
	if stamp != k.stamp || k.head.Load().Manifest.Gen != k.gen {
		l, err := k.readLocal(stamp)
		if err != nil {
			return err
		}
		if err := k.commit(ctx, l); err != nil {
			k.closeNew(l.open)
			return err
		}
	}
	return k.sweep(ctx, time.Now())
}

func (k *Keeper) readLocal(stamp uint64) (local, error) {
	parts, sources, err := trace.ListParts(k.cfg.Local)
	if err != nil {
		return local{}, err
	}
	l := local{parts: parts, sources: sources, open: map[string]*trace.Part{}, stamp: stamp}
	for _, lp := range parts {
		if p, ok := k.head.Load().Parts[lp.ID]; ok {
			l.open[lp.ID] = p
			continue
		}
		p, err := trace.OpenPartDir(lp.Dir)
		if err != nil {
			k.closeNew(l.open)
			return local{}, err
		}
		l.open[lp.ID] = p
	}
	return l, nil
}

func (k *Keeper) closeNew(open map[string]*trace.Part) {
	for id, p := range open {
		if k.head.Load().Parts[id] != p {
			k.closePart(p)
		}
	}
}

// commit uploads the cold parts the latest manifest does not name, validates
// the gen, then writes the manifest at (gen, seq+1) and dooms what it retired.
func (k *Keeper) commit(ctx context.Context, l local) error {
	prev := k.head.Load().Manifest
	named := map[string]PartRef{}
	for _, p := range prev.Parts {
		named[p.ID] = p
	}
	var refs, hot []PartRef
	for _, lp := range l.parts {
		if lp.Hot {
			hot = append(hot, PartRef{ID: lp.ID, Gen: k.gen, Sessions: lp.Sessions, ScoredDocs: lp.ScoredDocs, TotalLen: lp.TotalLen})
			continue
		}
		ref, ok := named[lp.ID]
		if !ok {
			ref, ok = k.uploaded[lp.ID]
		}
		if !ok {
			var err error
			if ref, err = k.upload(ctx, lp); err != nil {
				return err
			}
			k.uploaded[lp.ID] = ref
		}
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b PartRef) int { return strings.Compare(a.ID, b.ID) })
	next := Manifest{Tenant: k.cfg.Tenant, Gen: k.gen, Seq: 1, Fence: k.fence, Parts: refs, Sources: l.sources}
	if prev.Gen == k.gen {
		if slices.EqualFunc(prev.Parts, refs, func(a, b PartRef) bool { return a.Key == b.Key }) && bytes.Equal(prev.Sources, l.sources) {
			k.install(prev, hot, l)
			return nil
		}
		next.Seq = prev.Seq + 1
	}
	if _, err := k.check(ctx); err != nil {
		return err
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := k.s.Put(ctx, manifestKey(k.cfg.Tenant, next.Gen, next.Seq), bytes.NewReader(b), int64(len(b))); err != nil {
		return err
	}
	// A router serves a manifest only while its last List, started at r, still
	// shows it latest. A manifest that retires X was not visible at r, so its PUT
	// returned after r; a grant issued before r+DeleteSlack/2 dies by
	// r+GrantTTL+DeleteSlack/2, and its call by MaxCall later. notBefore counts
	// from this PUT's return, so it is past all of them.
	notBefore := time.Now().Add(k.fence.horizon())
	live := liveKeys(next)
	for _, p := range prev.Parts {
		k.doom(p.Key, notBefore, live)
	}
	for _, p := range k.uploaded {
		k.doom(p.Key, notBefore, live)
	}
	if prev.Gen == k.gen {
		k.doom(manifestKey(k.cfg.Tenant, prev.Gen, prev.Seq), notBefore, live)
	} else {
		k.takenAt = notBefore
	}
	k.uploaded = map[string]PartRef{}
	k.install(next, hot, l)
	return nil
}

func (k *Keeper) upload(ctx context.Context, lp trace.LocalPart) (PartRef, error) {
	f, err := os.CreateTemp(k.cfg.Local, ".pack-*")
	if err != nil {
		return PartRef{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	sum, size, err := trace.Pack(lp.Dir, f)
	if err != nil {
		return PartRef{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return PartRef{}, err
	}
	key := partKey(k.cfg.Tenant, lp.ID, k.gen)
	if err := k.s.Put(ctx, key, f, size); err != nil {
		return PartRef{}, err
	}
	return PartRef{ID: lp.ID, Gen: k.gen, Key: key, Size: size, SHA256: sum, Sessions: lp.Sessions, ScoredDocs: lp.ScoredDocs, TotalLen: lp.TotalLen}, nil
}

func liveKeys(m Manifest) map[obj.Key]bool {
	live := map[obj.Key]bool{manifestKey(m.Tenant, m.Gen, m.Seq): true}
	for _, p := range m.Parts {
		live[p.Key] = true
	}
	return live
}

func (k *Keeper) doom(key obj.Key, notBefore time.Time, live map[obj.Key]bool) {
	if _, ok := k.doomed[key]; !ok && !live[key] {
		k.doomed[key] = notBefore
	}
}

func (k *Keeper) install(m Manifest, hot []PartRef, l local) {
	k.mu.Lock()
	old, oldChanged := *k.head.Load(), k.changed
	k.head.Store(&Head{Manifest: m, Hot: hot, Parts: l.open, epoch: old.epoch + 1})
	k.stamp = l.stamp
	moved := m.Gen != old.Manifest.Gen || m.Seq != old.Manifest.Seq
	if moved {
		k.changed = make(chan struct{})
	}
	for id, p := range old.Parts {
		if l.open[id] != p {
			k.limbo = append(k.limbo, dropped{p, old.epoch})
		}
	}
	free := k.reap()
	k.mu.Unlock()
	if moved {
		close(oldChanged)
	}
	k.closeAll(free)
}

// reap takes out of limbo the parts no pinned head can still serve: those
// dropped at an epoch below every pinned one. k.mu is held.
func (k *Keeper) reap() []*trace.Part {
	oldest := ^uint64(0)
	for e := range k.pins {
		oldest = min(oldest, e)
	}
	var free []*trace.Part
	keep := k.limbo[:0]
	for _, d := range k.limbo {
		if d.epoch < oldest {
			free = append(free, d.p)
		} else {
			keep = append(keep, d)
		}
	}
	clear(k.limbo[len(keep):])
	k.limbo = keep
	return free
}

func (k *Keeper) closeAll(ps []*trace.Part) {
	for _, p := range ps {
		k.closePart(p)
	}
}

// sweep deletes the doomed objects whose notBefore has passed and that the
// committed manifest does not name, after validating the gen. At its first run
// and every rescanEvery after, it first dooms every older object the committed
// manifest does not name, which is how a crashed or stale keeper's garbage goes.
// The first scan shares the takeover commit's notBefore so no part is due before
// a manifest that names it; a rescan dooms at now plus the retire margin.
func (k *Keeper) sweep(ctx context.Context, now time.Time) error {
	m := k.head.Load().Manifest
	if m.Gen != k.gen {
		return nil
	}
	live := liveKeys(m)
	if k.scannedAt.IsZero() || now.Sub(k.scannedAt) >= rescanEvery {
		notBefore := now.Add(k.cfg.GrantTTL + k.cfg.DeleteSlack)
		if k.scannedAt.IsZero() {
			notBefore = k.takenAt
		}
		for _, prefix := range []obj.Key{manifestPrefix(k.cfg.Tenant), partPrefix(k.cfg.Tenant)} {
			es, err := k.s.List(ctx, prefix)
			if err != nil {
				return err
			}
			for _, e := range es {
				if g, ok := keyGen(k.cfg.Tenant, e.Key); ok && g < k.gen {
					k.doom(e.Key, notBefore, live)
				}
			}
		}
		k.scannedAt = now
	}
	var manifests, parts []obj.Key
	for key, t := range k.doomed {
		switch {
		case live[key]:
			delete(k.doomed, key)
		case !now.Before(t) && strings.HasPrefix(string(key), string(manifestPrefix(k.cfg.Tenant))):
			manifests = append(manifests, key)
		case !now.Before(t):
			parts = append(parts, key)
		}
	}
	if len(manifests)+len(parts) == 0 {
		return nil
	}
	if top, err := k.check(ctx); err != nil {
		if errors.Is(err, ErrLost) {
			fmt.Fprintf(os.Stderr, "keep: delete refused: %d objects due at gen %d, highest claimed gen is %d\n", len(manifests)+len(parts), k.gen, top)
		}
		return err
	}
	for _, keys := range [][]obj.Key{manifests, parts} {
		if len(keys) == 0 {
			continue
		}
		if err := k.s.Delete(ctx, keys); err != nil {
			return err
		}
		for _, key := range keys {
			delete(k.doomed, key)
		}
	}
	return nil
}

// Read calls fn with the head, which stays open until fn returns. The head is
// pinned, not locked: fn runs with no lock held, so a slow fn delays neither a
// commit nor another Read, and a part the commit drops closes after the last
// Read that saw it. It refuses once the last good gen check is older than
// 2*checkEvery.
func (k *Keeper) Read(fn func(Head) error) error {
	if err := k.admit(); err != nil {
		return err
	}
	k.mu.Lock()
	h := *k.head.Load()
	k.pins[h.epoch]++
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		if k.pins[h.epoch]--; k.pins[h.epoch] == 0 {
			delete(k.pins, h.epoch)
		}
		free := k.reap()
		k.mu.Unlock()
		k.closeAll(free)
	}()
	return fn(h)
}

// admit is the one gate every call passes: ErrLost once a newer keeper claimed
// the tenant, ErrUnchecked once the last good gen check is older than
// 2*checkEvery.
func (k *Keeper) admit() error {
	if k.lost.Load() {
		return ErrLost
	}
	if time.Since(*k.checked.Load()) > 2*k.cfg.checkEvery() {
		return ErrUnchecked
	}
	return nil
}

// Wait reports whether the committed (gen, seq) rose above (g, s) before ctx
// ended.
func (k *Keeper) Wait(ctx context.Context, g Gen, s Seq) bool {
	for {
		k.mu.Lock()
		m, ch := k.head.Load().Manifest, k.changed
		k.mu.Unlock()
		if m.Gen > g || (m.Gen == g && m.Seq > s) {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

const (
	restoreFanout = 16
	restoreGrant  = 10 * time.Minute
)

// restore makes k.cfg.Local cover every part of m, restoreFanout parts at a
// time, each fetched through a presigned GET and checked against its SHA256.
// Cycle runs it until the first commit at k.gen; RestorePart makes a rerun
// cheap. A part with no parse record is logged and skipped: its sources parse
// from raw. Any other failure fails the cycle after the rest have finished.
func (k *Keeper) restore(ctx context.Context, m Manifest) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	sem := make(chan struct{}, restoreFanout)
	for _, p := range m.Parts {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			_, err := k.restorePart(ctx, p)
			switch {
			case err == nil:
			case errors.Is(err, trace.ErrNoParseRecord):
				fmt.Fprintf(os.Stderr, "keep: restore part %s: %v\n", p.ID, err)
			default:
				mu.Lock()
				errs = append(errs, fmt.Errorf("restore part %s: %w", p.ID, err))
				mu.Unlock()
				cancel()
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return ctx.Err()
}

func (k *Keeper) restorePart(ctx context.Context, p PartRef) (int, error) {
	return trace.RestorePart(k.cfg.Local, p.ID, p.SHA256, func(w io.Writer) error {
		url, err := k.s.PresignGetUntil(p.Key, time.Now().Add(restoreGrant))
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: %s", p.Key, resp.Status)
		}
		_, err = io.Copy(w, resp.Body)
		return err
	})
}
