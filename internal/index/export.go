package index

// ExportSession is one session as the exports read it: its winner rows in
// event order, heavy columns included, plus its spans and annotations.
type ExportSession struct {
	ID          string
	Rows        []Row
	Payloads    []string
	TokenUsages []string
}

// EachSession calls fn for every session the export may read, in session id
// order; hidden reports sessions the session controls hide from the export.
func (db *DB) EachSession(hidden map[string]bool, fn func(ExportSession) error) error {
	panic("lane U5-export")
}
