package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const (
	quietFor       = 1 * time.Second
	flushWithin    = 5 * time.Second
	conflictRetry  = time.Second
	errorRetryMin  = 5 * time.Second
	errorRetryMax  = time.Minute
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
	hc       *http.Client
	globs    []watchGlob
	quiet    time.Duration
	within   time.Duration
	failed   bool
	failures int
	note     *notifier
	held     map[string]*wire.Source
	files    map[string]*watched
	dirs     map[string]bool
	stale    bool
	watchErr error
}

// Watch runs until ctx ends. Paths named by file events are sent once no
// event has arrived for quietFor, or flushWithin after the first unsent
// event, whichever comes first; a stats line is printed when something was
// sent or failed.
func Watch(ctx context.Context, cfg config.Client, red Redactor, hc *http.Client, out io.Writer) error {
	return newWatcher(cfg, red, hc).run(ctx, out)
}

func newWatcher(cfg config.Client, red Redactor, hc *http.Client) *watcher {
	w := &watcher{cfg: cfg, red: red, hc: hc, quiet: quietFor, within: flushWithin, stale: true}
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
	w.note = n
	periodic := time.NewTimer(0)
	defer periodic.Stop()
	dirty := make(map[string]bool)
	flushAt := time.NewTimer(time.Hour)
	flushAt.Stop()
	defer flushAt.Stop()
	var firstEvent time.Time
	var window, retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case p := <-n.changed:
			if p == lostEvents {
				w.stale = true
			} else {
				dirty[p] = true
			}
			now := time.Now()
			if window == nil {
				firstEvent = now
				window = flushAt.C
			}
			flushAt.Reset(min(w.quiet, firstEvent.Add(w.within).Sub(now)))
			continue
		case <-window:
		case <-retry:
			retry = nil
		case <-periodic.C:
			w.stale = true
		}
		flushAt.Stop()
		window = nil
		w.failed = false
		var st Stats
		var err error
		if w.stale {
			st, err = w.reconcile(ctx)
			periodic.Reset(reconcileEvery)
		} else {
			st = w.flush(ctx, dirty)
		}
		clear(dirty)
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
			w.failures = 0
		} else if retry == nil {
			retry = time.After(w.retryDelay(err))
		}
	}
}

// retryDelay is how long a stale watcher waits before it reconciles again.
// A 409 alone means the server's size moved, not that it is struggling, so it
// retries after conflictRetry. A failed request may mean an overloaded or
// absent server, so consecutive failures double the wait from errorRetryMin
// up to errorRetryMax.
func (w *watcher) retryDelay(reconcileErr error) time.Duration {
	if reconcileErr == nil && !w.failed {
		return conflictRetry
	}
	d := errorRetryMin << min(w.failures, 8)
	w.failures++
	return min(d, errorRetryMax)
}

func (w *watcher) reconcile(ctx context.Context) (Stats, error) {
	held, err := fetchManifest(ctx, w.cfg, w.hc)
	if err != nil {
		w.stale = true
		return Stats{}, err
	}
	oldFiles, oldDirs := w.files, w.dirs
	w.held = held
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
	return w.sync(ctx, paths), nil
}

// flush lists the dirty directories and any glob base not seen yet, then
// syncs the dirty files and the files those listings found.
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
		w.files[path] = &watched{fileRef: fileRef{path: path, harness: g.src.Harness, sourceName: g.src.SourceName}}
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

// sync watches each known path, then sends it when its (size, mtime) differs
// from its stamp at its last completed sync. A 409 or an error makes the next
// wake reconcile, after conflictRetry or a backoff.
func (w *watcher) sync(ctx context.Context, paths []string) Stats {
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
		f.size = now.size
		srcID := wire.SrcID(path)
		held, conflicted, err := syncSource(ctx, w.cfg, w.red, w.hc, w.held[srcID], f.fileRef, &st)
		if held != nil {
			w.held[srcID] = held
		}
		if conflicted || err != nil {
			w.stale = true
		}
		if err != nil {
			w.failed = true
			st.Failed++
			continue
		}
		f.synced = now
	}
	return st
}
