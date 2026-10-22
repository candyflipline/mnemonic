package keeper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
}

// checkEvery keeps 2*checkEvery, the longest Read serves after the last good
// check, below DeleteSlack, the margin a successor's deletes leave past the
// last grant; Take refuses a DeleteSlack under MinDeleteSlack.
func (c Config) checkEvery() time.Duration {
	return min(c.DeleteSlack/3, 30*time.Second)
}

// Head is what the keeper serves: its committed manifest, the hot parts that
// stay on its disk, every local part open by ID, and the source rows of the
// manifest, which get_ingest_status reads.
type Head struct {
	Manifest Manifest
	Hot      []PartRef
	Parts    map[string]*trace.Part
	status   *trace.DB
}

type Keeper struct {
	cfg     Config
	s       obj.Store
	gen     Gen
	lost    atomic.Bool
	checked atomic.Pointer[time.Time]

	cycle     sync.Mutex
	uploaded  map[string]PartRef
	doomed    map[obj.Key]time.Time
	scannedAt time.Time
	takenAt   time.Time

	mu      sync.RWMutex
	head    atomic.Pointer[Head]
	stamp   uint64
	changed chan struct{}

	closePart func(*trace.Part)
}

type local struct {
	parts   []trace.LocalPart
	sources json.RawMessage
	open    map[string]*trace.Part
	status  *trace.DB
	stamp   uint64
}

// Take checks that s enforces exclusive create, claims the next gen, then
// reads the latest manifest; the first commit is the first Cycle.
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
		uploaded:  map[string]PartRef{},
		doomed:    map[obj.Key]time.Time{},
		changed:   make(chan struct{}),
		closePart: (*trace.Part).Close,
	}
	status, err := trace.SourcesDB(m.Sources)
	if err != nil {
		return nil, err
	}
	k.head.Store(&Head{Manifest: m, status: status})
	k.checked.Store(&claimed)
	return k, nil
}

func (k *Keeper) Gen() Gen { return k.gen }

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
	if err := trace.Ingest(k.cfg.Root, k.cfg.Local, k.cfg.Quiet, k.cfg.Settle); err != nil {
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
	status, err := trace.SourcesDB(sources)
	if err != nil {
		return local{}, err
	}
	l := local{parts: parts, sources: sources, open: map[string]*trace.Part{}, status: status, stamp: stamp}
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
	next := Manifest{Tenant: k.cfg.Tenant, Gen: k.gen, Seq: 1, Parts: refs, Sources: l.sources}
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
	notBefore := time.Now().Add(k.cfg.GrantTTL + k.cfg.DeleteSlack)
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
	k.head.Store(&Head{Manifest: m, Hot: hot, Parts: l.open, status: l.status})
	k.stamp = l.stamp
	moved := m.Gen != old.Manifest.Gen || m.Seq != old.Manifest.Seq
	if moved {
		k.changed = make(chan struct{})
	}
	k.mu.Unlock()
	if moved {
		close(oldChanged)
	}
	for id, p := range old.Parts {
		if l.open[id] != p {
			k.closePart(p)
		}
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

// Read calls fn with the head, which stays open until fn returns. It refuses
// once the last good gen check is older than 2*checkEvery.
func (k *Keeper) Read(fn func(Head) error) error {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if err := k.admit(); err != nil {
		return err
	}
	return fn(*k.head.Load())
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

// Parts is every local part by ID; a part stays open while a Read that saw it
// runs.
func (k *Keeper) Parts() map[string]*trace.Part { return k.head.Load().Parts }

// Wait reports whether the committed (gen, seq) rose above (g, s) before ctx
// ended.
func (k *Keeper) Wait(ctx context.Context, g Gen, s Seq) bool {
	for {
		k.mu.RLock()
		m, ch := k.head.Load().Manifest, k.changed
		k.mu.RUnlock()
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

// IngestStatus answers get_ingest_status from the source rows of the head
// the last install made, the snapshot every Read serves, under the same gate
// as Read. It holds no lock.
func (k *Keeper) IngestStatus(args json.RawMessage) (json.RawMessage, string, error) {
	if err := k.admit(); err != nil {
		return nil, "", err
	}
	return trace.Call(k.head.Load().status, "get_ingest_status", args)
}
