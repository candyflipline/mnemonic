package index

// ARCHITECTURE SKETCH: server-only incremental entry, not a replacement for
// local mode's IngestPlain or the offline package Ingest entry.
import (
	"context"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/store"
)

// LiveIngest owns the complete source catalog used by publish, not just the
// dirty subset. Only its parse selection is sparse. Existing checkpoints
// remain authoritative; this first change does not cache all checkpoints.
type LiveIngest struct {
	ix      *Index
	raw     *store.Committer
	sources map[layout.Key]source
	retry   map[layout.Key]struct{}
}

func (*Index) OpenLive(context.Context, *store.Committer) (*LiveIngest, error) {
	// TODO: seed raw catalog from committer boot inventory, list archives once;
	// read checkpoints; seed boot/version work; do not list raw a second time.
	panic("not implemented")
}
func (*LiveIngest) Cycle(context.Context, IngestOptions) (IngestStats, error) {
	// TODO: Take changes; refresh only those source descriptors; open pinned raw
	// prefixes. Combine dirty, failed and version-sweep work; parse only those.
	// Keep ALL descriptors/checkpoints for publish: passing only dirty sources
	// to current ingest/publish would drop CURRENT members and GC live logs.
	// On partial failure requeue failed keys; on publication failure requeue
	// entire batch; a successful checkpoint never suppresses needed publication.
	// readCkpts(all), publish planning/archive-member reads/GC remain O(all).
	panic("not implemented")
}
