package index

import "sync"

// CycleStats is what one Ingest wrote, so a server cycle can prove an append
// is bounded by its component: bytes and parts written, by kind. LogBytes is
// the source frame logs' growth, PartBytes the parts/ tree's (a delta append's
// delta.json and its delta logs' growth), PartsBuilt the parts written; the
// hot and cold counts are the plan's, hot and settled components alike.
type CycleStats struct {
	LogBytes   int64
	PartBytes  int64
	PartsBuilt int
	HotParts   int
	ColdParts  int
}

// cycleRecord is everything one Ingest reports to the cycle that ran it, kept
// by store dir: the stats above, the sources the parser-version sweep left for
// the next cycle, the due components it left hot (settleSet), and the sources
// whose re-parse failed. The map is in memory
// on purpose, unlike the lake's on-disk parser marker: a failed sweep is
// retried once per process, and a push or the next version bump parses the
// source again regardless.
type cycleRecord struct {
	stats     CycleStats
	stale     int
	settle    int
	failed    int
	failSweep map[string]int
}

var (
	cycleMu    sync.Mutex
	lastCycles = map[string]cycleRecord{}
)

func recordCycle(store string, r cycleRecord) {
	cycleMu.Lock()
	lastCycles[store] = r
	cycleMu.Unlock()
}

func takeCycle(store string) cycleRecord {
	cycleMu.Lock()
	defer cycleMu.Unlock()
	return lastCycles[store]
}

func (ix *Index) LastCycle() CycleStats { return takeCycle(ix.store).stats }

// SweepStale is the work the last Ingest left for the next cycle: sources for
// the parser-version sweep and due components it did not settle. It is zero
// once both are done; the server runs cycles back to back while it is not.
func (ix *Index) SweepStale() int {
	r := takeCycle(ix.store)
	return r.stale + r.settle
}

// CycleFailed is the sources whose parse failed in the last Ingest; the cycle
// went on without them, and each stays on its older output.
func (ix *Index) CycleFailed() int { return takeCycle(ix.store).failed }
