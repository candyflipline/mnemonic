package index

import "sync"

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

func (ix *Index) SweepStale() int {
	r := takeCycle(ix.store)
	return r.stale + r.settle
}

func (ix *Index) CycleFailed() int { return takeCycle(ix.store).failed }
