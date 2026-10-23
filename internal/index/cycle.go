package index

// CycleStats is what one Ingest wrote, so a server cycle can prove an append
// is bounded by its component: bytes and parts written, by kind.
type CycleStats struct {
	LogBytes   int64
	PartBytes  int64
	PartsBuilt int
	HotParts   int
	ColdParts  int
}

// LastCycle is the CycleStats of the last Ingest through ix.
func (ix *Index) LastCycle() CycleStats { panic("lane U3") }
