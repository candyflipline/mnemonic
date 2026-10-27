package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/store"
)

// Lap is the server's index between cycles: the source list and checkpoints
// the boot listing read, kept in listing order (manifest path, then
// archive files), patched by each cycle's changed keys. Boot is the only way
// to one, so no cycle can skip the full listing it depends on.
//
// Its premise: after boot, raw/ changes only through the store's committer,
// which reports every key it changed, and the index store changes only
// through this Lap, which holds its dir for the server's life. Archive files
// (machine=archive-ch) are written by nothing, so the boot listing is their
// only one. Anything else that writes raw/ needs a restart.
type Lap struct {
	ix      *Index
	root    string
	sources *sourceSet
	carry   map[layout.Key]bool
}

// Boot is today's cycle over a full listing (the store's manifests,
// readCkpts), and keeps what it read. The Lap comes back whenever the listing
// and checkpoints were read, even when the cycle failed: a later Next
// publishes again. manifests is the store's table: the committer has already
// read every manifest file once at Open, so boot does not read them again.
func (ix *Index) Boot(root string, manifests []store.Manifest, opts IngestOptions) (*Lap, IngestStats, error) {
	l := &Lap{ix: ix, root: root}
	var c cycled
	err := withWriterLock(ix.store, func(w writerLock) error {
		sources, err := sourcesOf(root, manifests)
		if err != nil {
			return err
		}
		readCkpts(ix.store, sources.list)
		l.sources = sources
		c = l.cycle(w, allOf(sources.list), opts)
		return nil
	})
	if err != nil {
		return nil, IngestStats{}, err
	}
	stats, err := l.serve(c)
	return l, stats, err
}

func (l *Lap) Next(changed []layout.Key, opts IngestOptions) (IngestStats, error) {
	for _, k := range changed {
		l.carry[k] = true
	}
	var c cycled
	var patchErr error
	err := withWriterLock(l.ix.store, func(w writerLock) error {
		if patchErr = l.patch(l.carry); patchErr == nil {
			c = l.cycle(w, l.work(), opts)
		}
		return nil
	})
	if err != nil {
		return IngestStats{}, err
	}
	if patchErr != nil {
		return IngestStats{}, errors.Join(patchErr, l.ix.Refresh())
	}
	return l.serve(c)
}

func (l *Lap) work() []int {
	want := make(map[string]bool, len(l.carry))
	for k := range l.carry {
		want[sourceKey(k)] = true
	}
	var work []int
	for i, s := range l.sources.list {
		if want[sourceKey(s.key)] || versionStale(s) {
			work = append(work, i)
		}
	}
	return work
}

type cycled struct {
	work     []int
	stats    IngestStats
	outcomes map[string]Outcome
	opened   map[string]*Part
	err      error
}

func (l *Lap) cycle(w writerLock, work []int, opts IngestOptions) cycled {
	c := cycled{work: work}
	c.stats, c.outcomes, c.opened, c.err = l.sources.cycle(w, l.root, work, opts.Quiet, opts.SettleAll, opts.Red)
	return c
}

// serve serves what a cycle published, after its writer lock is released.
// carry becomes the keys among its work whose parse did not complete, or all
// of the work when the publish or the serve failed: their checkpoints may
// have moved past what CURRENT names.
func (l *Lap) serve(c cycled) (IngestStats, error) {
	err := errors.Join(c.err, l.ix.refresh(c.opened))
	l.carry = map[layout.Key]bool{}
	for _, i := range c.work {
		if _, ok := c.outcomes[sourceKey(l.sources.list[i].key)]; !ok || err != nil {
			l.carry[l.sources.list[i].key] = true
		}
	}
	return c.stats, err
}

func (l *Lap) patch(changed map[layout.Key]bool) error {
	for k := range changed {
		list := l.sources.list
		n := sort.Search(len(list), func(i int) bool { return list[i].kind == KindArchive })
		p := k.Manifest(l.root)
		i, found := slices.BinarySearchFunc(list[:n], p, func(s source, p string) int { return strings.Compare(s.mpath, p) })
		m, ok, err := readSourceManifest(p)
		if err != nil {
			return err
		}
		switch {
		case ok && found:
			list[i].src = m
		case ok:
			s := storeSource(l.root, store.Manifest{Key: k, Source: m.Source, ZstSize: m.ZstSize})
			if have, ok := readCkpt(l.ix.store, sourceKey(k)); ok {
				s.ck = &have
			}
			if err := l.sources.insert(i, s); err != nil {
				return err
			}
		case found:
			l.sources.remove(i)
		}
	}
	return nil
}

func readSourceManifest(path string) (manifest, bool, error) {
	var m manifest
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		fmt.Fprintf(os.Stderr, "ingest: %s: %v\n", path, err)
		return m, false, nil
	}
	return m, true, nil
}
