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
)

// Lap is the server's index between cycles: the source list and checkpoints
// the boot listing read, kept in listSources order (manifest path, then
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
	sources []source
	cks     []*ckpt // cks[i] is sources[i]'s checkpoint, nil when none
	carry   map[layout.Key]bool
}

// Boot is today's cycle over a full listing (listSources, readCkpts), and
// keeps what it read. The Lap comes back whenever the listing and checkpoints
// were read, even when the cycle failed: a later Next publishes again.
func (ix *Index) Boot(root string, opts IngestOptions) (*Lap, IngestStats, error) {
	sources, err := listSources(root)
	if err != nil {
		return nil, IngestStats{}, err
	}
	if err := oneKeyEach(sources); err != nil {
		return nil, IngestStats{}, err
	}
	release, err := IngestLock(ix.store)
	if err != nil {
		return nil, IngestStats{}, err
	}
	l := &Lap{ix: ix, root: root, sources: sources, cks: readCkpts(ix.store, sources)}
	stats, err := l.cycle(allOf(sources), opts)
	release()
	return l, stats, errors.Join(err, ix.Refresh())
}

// Next runs one cycle over every source but parses only changed, carry (the
// keys whose manifest read or parse failed last time) and the version-stale
// ones the sweep picks from the cached checkpoints. Every other source is
// Unchanged without a read. publish still sees every source, and its settle
// writes land in l.cks.
func (l *Lap) Next(changed []layout.Key, opts IngestOptions) (IngestStats, error) {
	for _, k := range changed {
		l.carry[k] = true
	}
	release, err := IngestLock(l.ix.store)
	if err != nil {
		return IngestStats{}, err
	}
	var stats IngestStats
	if err = l.patch(l.carry); err == nil {
		stats, err = l.cycle(l.work(), opts)
	}
	release()
	return stats, errors.Join(err, l.ix.Refresh())
}

// work is the sources a cycle after boot parses: the carried keys (changed
// ones included) and the version-stale ones.
func (l *Lap) work() []int {
	want := make(map[string]bool, len(l.carry))
	for k := range l.carry {
		want[sourceKey(k)] = true
	}
	var work []int
	for i, s := range l.sources {
		if want[sourceKey(s.key)] || versionStale(s, l.cks[i]) {
			work = append(work, i)
		}
	}
	return work
}

// cycle parses the sources work names and publishes all of them; carry
// becomes the keys among work whose parse did not complete.
func (l *Lap) cycle(work []int, opts IngestOptions) (IngestStats, error) {
	stats, outcomes, err := cycle(l.root, l.ix.store, l.sources, l.cks, work, opts.Quiet, opts.SettleAll, opts.Red)
	l.carry = map[layout.Key]bool{}
	for _, i := range work {
		if _, ok := outcomes[sourceKey(l.sources[i].key)]; !ok {
			l.carry[l.sources[i].key] = true
		}
	}
	return stats, err
}

// patch re-reads the manifest of each changed key: a new key is inserted at
// its sorted position with the checkpoint the store has for it, a changed one
// replaced, one whose manifest is gone (a replace crashed between unlink and
// rename) or does not parse removed, as the full listing would. A key that is
// not a raw source (an archive file's) reads no manifest and changes nothing.
func (l *Lap) patch(changed map[layout.Key]bool) error {
	for k := range changed {
		n := sort.Search(len(l.sources), func(i int) bool { return l.sources[i].kind == KindArchive })
		p := k.Manifest(l.root)
		i, found := slices.BinarySearchFunc(l.sources[:n], p, func(s source, p string) int { return strings.Compare(s.mpath, p) })
		m, ok, err := readSourceManifest(p)
		if err != nil {
			return err
		}
		switch {
		case ok && found:
			l.sources[i].src = m
		case ok:
			s := source{kind: KindStore, mpath: p, key: k, src: m}
			var c *ckpt
			if have, ok := readCkpt(l.ix.store, sourceKey(k)); ok {
				c = &have
			}
			l.sources = slices.Insert(l.sources, i, s)
			l.cks = slices.Insert(l.cks, i, c)
		case found:
			l.sources = slices.Delete(l.sources, i, i+1)
			l.cks = slices.Delete(l.cks, i, i+1)
		}
	}
	return nil
}

// readSourceManifest is one raw manifest as listSources reads it: false when
// it is gone, or does not parse (said on stderr, and the source skipped).
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
