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
// (machine=archive-ch) are written by nothing since import-ch retired, so the
// boot listing is their only one. Anything else that writes raw/ needs a
// restart.
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
	sources, err := sourcesOf(root, manifests)
	if err != nil {
		return nil, IngestStats{}, err
	}
	release, err := IngestLock(ix.store)
	if err != nil {
		return nil, IngestStats{}, err
	}
	readCkpts(ix.store, sources.list)
	l := &Lap{ix: ix, root: root, sources: sources}
	stats, err := l.cycle(allOf(sources.list), opts, release)
	return l, stats, err
}

func (l *Lap) Next(changed []layout.Key, opts IngestOptions) (IngestStats, error) {
	for _, k := range changed {
		l.carry[k] = true
	}
	release, err := IngestLock(l.ix.store)
	if err != nil {
		return IngestStats{}, err
	}
	if err := l.patch(l.carry); err != nil {
		release()
		return IngestStats{}, errors.Join(err, l.ix.Refresh())
	}
	return l.cycle(l.work(), opts, release)
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

// cycle parses the sources work names, publishes all of them, releases the
// writer lock and serves the result. carry becomes the keys among work whose
// parse did not complete, or all of work when the publish or the serve failed:
// their checkpoints may have moved past what CURRENT names.
func (l *Lap) cycle(work []int, opts IngestOptions, release func()) (IngestStats, error) {
	stats, outcomes, opened, err := cycle(l.root, l.ix.store, l.sources.list, work, opts.Quiet, opts.SettleAll, opts.Red)
	release()
	err = errors.Join(err, l.ix.refresh(opened))
	l.carry = map[layout.Key]bool{}
	for _, i := range work {
		if _, ok := outcomes[sourceKey(l.sources.list[i].key)]; !ok || err != nil {
			l.carry[l.sources.list[i].key] = true
		}
	}
	return stats, err
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

// readSourceManifest is one raw manifest: false when it is gone, or does not
// parse (said on stderr).
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
