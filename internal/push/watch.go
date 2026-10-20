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
	tickEvery      = time.Minute
	reconcileEvery = time.Hour
)

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
	cfg        config.Client
	red        Redactor
	hc         *http.Client
	globs      []watchGlob
	held       map[string]*wire.Source
	files      map[string]*watched
	dirs       map[string]int64
	reconciled time.Time
	stale      bool
}

// Watch runs until ctx ends, polling every minute and printing a stats line
// for each tick that sent or failed something.
func Watch(ctx context.Context, cfg config.Client, red Redactor, hc *http.Client, out io.Writer) error {
	w := newWatcher(cfg, red, hc)
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		st, err := w.tick(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			fmt.Fprintln(out, "push:", err)
		case st.Appended+st.Replaced+st.Failed > 0:
			fmt.Fprintln(out, st)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func newWatcher(cfg config.Client, red Redactor, hc *http.Client) *watcher {
	w := &watcher{cfg: cfg, red: red, hc: hc, stale: true}
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

func (w *watcher) tick(ctx context.Context) (Stats, error) {
	if w.stale || time.Since(w.reconciled) >= reconcileEvery {
		return w.reconcile(ctx)
	}
	var changed []string
	for dir, mtime := range w.dirs {
		info, err := os.Stat(dir)
		if err != nil {
			delete(w.dirs, dir)
			continue
		}
		if info.ModTime().UnixNano() != mtime {
			changed = append(changed, dir)
		}
	}
	for _, g := range w.globs {
		if _, ok := w.dirs[g.base]; !ok {
			changed = append(changed, g.base)
		}
	}
	for _, dir := range changed {
		w.list(dir)
	}
	return w.syncChanged(ctx), nil
}

func (w *watcher) reconcile(ctx context.Context) (Stats, error) {
	held, err := fetchManifest(ctx, w.cfg, w.hc)
	if err != nil {
		w.stale = true
		return Stats{}, err
	}
	w.held = held
	w.files = make(map[string]*watched)
	w.dirs = make(map[string]int64)
	for _, f := range scan(w.cfg.Sources) {
		w.files[f.path] = &watched{fileRef: f}
	}
	for _, g := range w.globs {
		w.list(g.base)
	}
	w.reconciled, w.stale = time.Now(), false
	return w.syncChanged(ctx), nil
}

// list reads one directory level, adding new matching files and listing new
// subdirectories. The stamp is taken before the read, so a change during the
// read shows as a new mtime next tick.
func (w *watcher) list(dir string) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		delete(w.dirs, dir)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		w.dirs[dir] = 0
		return
	}
	w.dirs[dir] = info.ModTime().UnixNano()
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			if _, ok := w.dirs[path]; !ok {
				w.list(path)
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

// syncChanged sends every file whose (size, mtime) differs from its stamp at
// its last completed sync. A 409 or an error makes the next tick reconcile.
func (w *watcher) syncChanged(ctx context.Context) Stats {
	var st Stats
	for path, f := range w.files {
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			delete(w.files, path)
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
			st.Failed++
			continue
		}
		f.synced = now
	}
	return st
}
