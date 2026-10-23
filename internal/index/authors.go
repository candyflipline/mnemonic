package index

// buildSessionAuthors is a part's session authors section: per session, every
// author (from the source key) of a source that emitted a row of it, winner or
// not, sorted and distinct. It runs at part build time over the rows before
// winner selection.
func buildSessionAuthors(members []ckpt, rows []built) map[string][]string {
	panic("lane U5-authors")
}

// sessionAuthors is sessionID's entry in its part's session authors section,
// nil for a session db does not hold.
func (db *DB) sessionAuthors(sessionID string) []string { panic("lane U5-authors") }
