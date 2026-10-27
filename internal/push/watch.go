package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const (
	quietFor      = 1 * time.Second
	flushWithin   = 5 * time.Second
	errorRetryMin = 5 * time.Second
	errorRetryMax = time.Minute
	// reconcileEvery is local mode's (WatchSink) full rescan; the push
	// stream needs none, because Welcome supplies the server's state.
	reconcileEvery = 6 * time.Hour
)

const lostEvents = ""

type stamp struct{ size, mtimeNs int64 }

type watched struct {
	fileRef
	synced stamp
}

type watchGlob struct {
	src        config.SourceGlob
	glob, base string
}

type watcher struct {
	cfg      config.Client
	red      Redactor
	up       *uplink
	sink     Sink
	globs    []watchGlob
	quiet    time.Duration
	within   time.Duration
	failed   bool
	retries  int
	note     *notifier
	files    map[string]*watched
	deferred map[string]bool
	dirs     map[string]bool
	stale    bool
	watchErr error
}

// Sink is where a watcher hands the paths whose (size, mtime) changed since
// their last sync, instead of the push stream: local mode feeds them to the
// index ingest. Sync brings the sink's index up to the named files and
// reports what it did; an error fails the pass, and the watcher retries on
// the same backoff a failed push gets.
type Sink interface {
	Sync(ctx context.Context, paths []string) (Stats, error)
}

// Watch runs until ctx ends. Paths named by file events are sent at the first
// quietFor tick after the first unsent event that finds no newer event, or
// flushWithin after it, whichever comes first; a stats line is printed when
// something was sent or failed. It holds one push stream open and, whenever
// the stream ends, backs off, redials and runs one full pass.
func Watch(ctx context.Context, cfg config.Client, red Redactor, out io.Writer) error {
	return newWatcher(cfg, red).run(ctx, out)
}

// WatchSink is Watch with every changed path handed to sink instead of pushed: no server is contacted, nothing is redacted, and the sink owns
// its own sync state — it is told, it is not kept current. Syncing still
// follows the watcher's quiet window and stamp tests.
func WatchSink(ctx context.Context, cfg config.Client, sink Sink, out io.Writer) error {
	w := newWatcher(cfg, nil)
	w.sink = sink
	return w.run(ctx, out)
}

func newWatcher(cfg config.Client, red Redactor) *watcher {
	w := &watcher{cfg: cfg, red: red, quiet: quietFor, within: flushWithin, stale: true, deferred: make(map[string]bool)}
	for _, src := range cfg.Sources {
		glob, err := filepath.Abs(src.Glob)
		if err != nil {
			continue
		}
		base, _ := doublestar.SplitPattern(filepath.ToSlash(glob))
		w.globs = append(w.globs, watchGlob{src: src, glob: glob, base: filepath.FromSlash(base)})
	}
	return w
}

func (w *watcher) run(ctx context.Context, out io.Writer) error {
	n, err := newNotifier()
	if err != nil {
		return err
	}
	defer n.close()
	defer w.hangup()
	w.note = n
	dirty := make(map[string]bool)
	flushAt := time.NewTimer(time.Hour)
	flushAt.Stop()
	defer flushAt.Stop()
	mark := func(p string) {
		if p == lostEvents {
			w.stale = true
		} else {
			dirty[p] = true
		}
	}
	var firstEvent time.Time
	var window <-chan time.Time
	retry := time.After(0)
	var periodic <-chan time.Time
	var periodicT *time.Timer
	if w.sink != nil {
		periodicT = time.NewTimer(reconcileEvery)
		periodic = periodicT.C
		defer periodicT.Stop()
	}
	var fresh, delivered bool
	for {
		var acks <-chan ack
		var ended <-chan struct{}
		if w.up != nil {
			acks, ended = w.up.acked, w.up.ended()
		}
		select {
		case <-ctx.Done():
			return nil
		case p := <-n.changed:
			mark(p)
			delivered = true
			if window != nil {
				fresh = true
				continue
			}
			firstEvent = time.Now()
			window = flushAt.C
			flushAt.Reset(w.quiet)
			continue
		case <-window:
			for _, p := range n.drain() {
				mark(p)
				fresh = true
			}
			if left := w.within - time.Since(firstEvent); fresh && left > 0 {
				fresh = false
				flushAt.Reset(min(w.quiet, left))
				continue
			}
			if retry != nil {
				window, fresh = nil, false
				continue
			}
		case <-retry:
			retry = nil
		case <-periodic:
			w.stale = true
		case a := <-acks:
			if err := w.up.consume(a); err != nil {
				fmt.Fprintln(out, "push:", err)
			}
			w.takeRedo(dirty)
			if window == nil && w.ready(dirty) {
				firstEvent = time.Now()
				window = flushAt.C
				flushAt.Reset(w.quiet)
			}
			continue
		case <-ended:
			if err := w.up.cause(); err != io.EOF {
				fmt.Fprintln(out, "push:", err)
			} else {
				fmt.Fprintln(out, "push: stream closed by the server")
			}
			w.up, w.stale = nil, true
			if retry == nil {
				retry = time.After(w.retryDelay())
			}
			continue
		}
		flushAt.Stop()
		window, fresh = nil, false
		w.failed = false
		var st Stats
		var err error
		if w.sink == nil && w.up == nil {
			if w.up, err = dial(ctx, w.cfg); err == nil {
				w.stale = true
			}
		}
		switch {
		case err != nil:
			w.stale = true
		case w.stale:
			st = w.reconcile(ctx)
			if periodicT != nil {
				periodicT.Reset(reconcileEvery)
			}
		default:
			st = w.flush(ctx, dirty)
		}
		clear(dirty)
		for p := range w.deferred {
			dirty[p] = true
		}
		clear(w.deferred)
		w.takeRedo(dirty)
		if delivered {
			n.rearm()
			delivered = false
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			fmt.Fprintln(out, "push:", err)
		}
		if w.watchErr != nil {
			fmt.Fprintln(out, "push:", w.watchErr)
			w.watchErr = nil
		}
		if st.Appended+st.Replaced+st.Failed > 0 {
			fmt.Fprintln(out, st)
		}
		if !w.stale {
			w.retries = 0
		} else if retry == nil {
			retry = time.After(w.retryDelay())
		}
		if window == nil && w.ready(dirty) {
			firstEvent = time.Now()
			window = flushAt.C
			flushAt.Reset(w.quiet)
		}
	}
}

// hangup ends the stream without waiting for acks: what was sent and not
// acked is forgotten, and the next Welcome says what landed.
func (w *watcher) hangup() {
	if w.up != nil {
		w.up.cancel()
	}
}

func (w *watcher) takeRedo(dirty map[string]bool) {
	if w.up == nil {
		return
	}
	for _, p := range w.up.takeRedo() {
		if f := w.files[p]; f != nil {
			f.synced = stamp{}
		}
		dirty[p] = true
	}
}

func (w *watcher) ready(dirty map[string]bool) bool {
	for p := range dirty {
		if w.up == nil || !w.up.busy(wire.SrcID(p)) {
			return true
		}
	}
	return false
}

func (w *watcher) retryDelay() time.Duration {
	d := errorRetryMin << min(w.retries, 8)
	w.retries++
	return min(d, errorRetryMax)
}

func (w *watcher) reconcile(ctx context.Context) Stats {
	oldFiles, oldDirs := w.files, w.dirs
	w.files = make(map[string]*watched)
	w.dirs = make(map[string]bool)
	w.stale = false
	for _, f := range scan(w.cfg.Sources) {
		w.files[f.path] = &watched{fileRef: f}
	}
	for _, g := range w.globs {
		w.list(g.base, nil)
	}
	for p := range oldFiles {
		if w.files[p] == nil {
			w.note.forget(p)
		}
	}
	for p := range oldDirs {
		if !w.dirs[p] {
			w.note.forget(p)
		}
	}
	paths := make([]string, 0, len(w.files))
	for p := range w.files {
		paths = append(paths, p)
	}
	return w.sync(ctx, paths)
}

func (w *watcher) flush(ctx context.Context, dirty map[string]bool) Stats {
	var paths []string
	for p := range dirty {
		if w.dirs[p] {
			paths = w.list(p, paths)
		}
	}
	for _, g := range w.globs {
		if !w.dirs[g.base] {
			paths = w.list(g.base, paths)
		}
	}
	for p := range dirty {
		if !w.dirs[p] {
			paths = append(paths, p)
		}
	}
	return w.sync(ctx, paths)
}

// list watches dir, then reads it, adding new matching files and listing new
// subdirectories, and returns added with the new files appended. Watching
// before reading means an entry added after the read raises an event.
func (w *watcher) list(dir string, added []string) []string {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		delete(w.dirs, dir)
		w.note.forget(dir)
		return added
	}
	w.watch(dir, true)
	w.dirs[dir] = true
	entries, err := os.ReadDir(dir)
	if err != nil {
		return added
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if !w.dirs[path] {
				added = w.list(path, added)
			}
			continue
		}
		if _, ok := w.files[path]; ok {
			continue
		}
		g, ok := w.classify(path)
		if !ok {
			continue
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		w.files[path] = &watched{fileRef: fileRef{path: path, harness: g.src.Harness, sourceName: g.src.SourceName, head: new(rawHead)}}
		added = append(added, path)
	}
	return added
}

func (w *watcher) watch(path string, dir bool) {
	err := w.note.watch(path, dir)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	w.stale = true
	if w.watchErr == nil {
		w.watchErr = err
	}
}

func (w *watcher) classify(path string) (watchGlob, bool) {
	for _, g := range w.globs {
		if ok, _ := doublestar.PathMatch(g.glob, path); ok {
			return g, true
		}
	}
	return watchGlob{}, false
}

func (w *watcher) sync(ctx context.Context, paths []string) Stats {
	if w.sink != nil {
		return w.syncSink(ctx, paths)
	}
	var st Stats
	for _, path := range paths {
		f := w.files[path]
		if f == nil {
			continue
		}
		w.watch(path, false)
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			delete(w.files, path)
			w.note.forget(path)
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		now := stamp{size: info.Size(), mtimeNs: info.ModTime().UnixNano()}
		if now == f.synced {
			continue
		}
		if w.up.busy(wire.SrcID(path)) {
			w.deferred[path] = true
			continue
		}
		st.Scanned++
		f.size, f.mtimeMs = now.size, info.ModTime().UnixMilli()
		if err := streamSource(ctx, w.red, w.up, f.fileRef, &st); err != nil {
			w.stale, w.failed = true, true
			st.Failed++
			var se streamErr
			if errors.As(err, &se) {
				return st
			}
			continue
		}
		f.synced = now
	}
	return st
}

func (w *watcher) syncSink(ctx context.Context, paths []string) Stats {
	type due struct {
		path  string
		mtime int64
		at    stamp
	}
	var changed []due
	var st Stats
	for _, path := range paths {
		f := w.files[path]
		if f == nil {
			continue
		}
		w.watch(path, false)
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			delete(w.files, path)
			w.note.forget(path)
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		now := stamp{size: info.Size(), mtimeNs: info.ModTime().UnixNano()}
		if now == f.synced {
			continue
		}
		st.Scanned++
		f.size, f.mtimeMs = now.size, info.ModTime().UnixMilli()
		changed = append(changed, due{path: path, mtime: f.mtimeMs, at: now})
	}
	if len(changed) == 0 {
		return st
	}
	names := make([]string, len(changed))
	for i, d := range changed {
		names[i] = d.path
	}
	res, err := w.sink.Sync(ctx, names)
	st.Appended, st.Replaced, st.Unchanged, st.Bytes = res.Appended, res.Replaced, res.Unchanged, res.Bytes
	if err != nil {
		w.failed = true
		w.stale = true
		st.Failed = len(changed)
		return st
	}
	for _, d := range changed {
		if f := w.files[d.path]; f != nil {
			f.mtimeMs = d.mtime
			f.synced = d.at
		}
	}
	return st
}
