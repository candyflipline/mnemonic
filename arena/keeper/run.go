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

const (
	checkEvery  = 30 * time.Second
	deleteSlack = time.Minute
)

type Config struct {
	Tenant   string
	Root     string
	Local    string
	Quiet    time.Duration
	Settle   bool
	GrantTTL time.Duration
}

// Head is what the keeper serves: its committed manifest, the hot parts that
// stay on its disk, and every local part open by ID.
type Head struct {
	Manifest Manifest
	Hot      []PartRef
	Parts    map[string]*trace.Part
}

type Keeper struct {
	cfg  Config
	s    obj.Store
	gen  Gen
	lost atomic.Bool

	cycle    sync.Mutex
	uploaded map[string]PartRef
	doomed   map[obj.Key]time.Time
	scanned  bool

	mu      sync.RWMutex
	head    Head
	stamp   uint64
	changed chan struct{}
}

type local struct {
	parts   []trace.LocalPart
	sources json.RawMessage
	open    map[string]*trace.Part
	stamp   uint64
}

// Take claims the next gen, then reads the latest manifest; the first commit
// is the first Cycle.
func Take(ctx context.Context, s obj.Store, cfg Config) (*Keeper, error) {
	g, err := Claim(ctx, s, cfg.Tenant)
	if err != nil {
		return nil, err
	}
	m, _, err := Latest(ctx, s, cfg.Tenant)
	if err != nil {
		return nil, err
	}
	return &Keeper{
		cfg:      cfg,
		s:        s,
		gen:      g,
		uploaded: map[string]PartRef{},
		doomed:   map[obj.Key]time.Time{},
		head:     Head{Manifest: m},
		changed:  make(chan struct{}),
	}, nil
}

func (k *Keeper) Gen() Gen { return k.gen }

// Check fails with ErrLost once a higher gen is claimed, and keeps failing.
func (k *Keeper) Check(ctx context.Context) error {
	if k.lost.Load() {
		return ErrLost
	}
	ok, err := Current(ctx, k.s, k.cfg.Tenant, k.gen)
	if err != nil {
		return err
	}
	if !ok {
		k.lost.Store(true)
		return ErrLost
	}
	return nil
}

// Run cycles every interval and checks the gen every 30 s until ctx ends or
// the gen is lost.
func (k *Keeper) Run(ctx context.Context, every time.Duration) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		t := time.NewTicker(checkEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := k.Check(ctx); errors.Is(err, ErrLost) {
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
	if stamp != k.stamp || k.head.Manifest.Gen != k.gen {
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
		if p, ok := k.head.Parts[lp.ID]; ok {
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
		if k.head.Parts[id] != p {
			p.Close()
		}
	}
}

// commit uploads the cold parts the latest manifest does not name, validates
// the gen, then writes the manifest at (gen, seq+1) and dooms what it retired.
func (k *Keeper) commit(ctx context.Context, l local) error {
	prev := k.head.Manifest
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
	if err := k.Check(ctx); err != nil {
		return err
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := k.s.Put(ctx, manifestKey(k.cfg.Tenant, next.Gen, next.Seq), bytes.NewReader(b), int64(len(b))); err != nil {
		return err
	}
	notBefore := time.Now().Add(k.cfg.GrantTTL + deleteSlack)
	live := liveKeys(next)
	for _, p := range prev.Parts {
		k.doom(p.Key, notBefore, live)
	}
	for _, p := range k.uploaded {
		k.doom(p.Key, notBefore, live)
	}
	if prev.Gen == k.gen {
		k.doom(manifestKey(k.cfg.Tenant, prev.Gen, prev.Seq), notBefore, live)
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
	old, oldChanged := k.head, k.changed
	k.head = Head{Manifest: m, Hot: hot, Parts: l.open}
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
			p.Close()
		}
	}
}

// sweep deletes the doomed objects whose notBefore has passed and that the
// committed manifest does not name, after validating the gen. Once per gen it
// first dooms every older object the committed manifest does not name, which
// is how a crashed keeper's garbage goes.
func (k *Keeper) sweep(ctx context.Context, now time.Time) error {
	m := k.head.Manifest
	if m.Gen != k.gen {
		return nil
	}
	live := liveKeys(m)
	if !k.scanned {
		notBefore := now.Add(k.cfg.GrantTTL + deleteSlack)
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
		k.scanned = true
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
	if err := k.Check(ctx); err != nil {
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

// Read calls fn with the head, which stays open until fn returns.
func (k *Keeper) Read(fn func(Head) error) error {
	if k.lost.Load() {
		return ErrLost
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return fn(k.head)
}

// Wait returns once the committed (gen, seq) is above (g, s), or ctx ends.
func (k *Keeper) Wait(ctx context.Context, g Gen, s Seq) {
	for {
		k.mu.RLock()
		m, ch := k.head.Manifest, k.changed
		k.mu.RUnlock()
		if m.Gen > g || (m.Gen == g && m.Seq > s) {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

// IngestStatus answers get_ingest_status from the local store's committed
// sources, between cycles.
func (k *Keeper) IngestStatus(args json.RawMessage) (json.RawMessage, string, error) {
	k.cycle.Lock()
	defer k.cycle.Unlock()
	db, err := trace.OpenDB(k.cfg.Local)
	if err != nil {
		return nil, "", err
	}
	defer db.Close()
	return trace.Call(db, "get_ingest_status", args)
}
