package index

// SKETCH (docs/design/uploader.md, "Dirty-set handoff"): signatures only.

import (
	"github.com/candyflipline/mnemonic/internal/layout"
)

// Lap is the server's index between cycles: the source list and checkpoints
// the boot listing read, kept in listSources order (manifest path, then
// archive files), patched by each cycle's changed keys. Boot is the only way
// to one, so no cycle can skip the full listing it depends on.
//
// Its premise: after boot, raw/ changes only through the store's committer,
// which reports every key it changed. Archive files (machine=archive-ch) are
// written by nothing since import-ch retired, so the boot listing is their
// only one. Anything else that writes raw/ needs a restart.
type Lap struct {
	ix      *Index
	root    string
	sources []source
	cks     []*ckpt        // cks[i] is sources[i]'s checkpoint, nil when none
	at      map[string]int // sourceKey -> index in sources
	carry   map[string]bool
}

// Boot is today's cycle over a full listing (listSources, readCkpts), and
// keeps what it read.
func (ix *Index) Boot(root string, opts IngestOptions) (*Lap, IngestStats, error) {
	panic("not implemented")
}

// Next runs one cycle over every source but parses only changed, carry (the
// sources whose parse failed last time) and the version-stale ones the sweep
// picks from the cached checkpoints. Every other source is Unchanged without
// a read. publish still sees every source; its settle writes update l.cks.
func (l *Lap) Next(changed []layout.Key, opts IngestOptions) (IngestStats, error) {
	// TODO:
	// l.patch(changed)
	// work := changed ∪ carry ∪ {i : versionStale(sources[i], cks[i])}
	// outcomes := parseSources over work only (cks updated in place)
	// stats[Unchanged] += len(sources) - len(work)
	// publish(store, l.sources, l.cks, ...) with settled checkpoints written
	//   back into l.cks (today publish updates a copy, ingest.go:796-805)
	// l.carry = work keys with no outcome
	// ix.Refresh()
	panic("not implemented")
}

// patch re-reads the manifest of each changed key: a new key is inserted at
// its sorted position, a changed one replaced, one whose manifest is gone
// (a replace crashed between unlink and rename) removed, as the full listing
// would.
func (l *Lap) patch(changed []layout.Key) error { panic("not implemented") }
