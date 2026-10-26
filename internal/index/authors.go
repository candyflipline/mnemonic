package index

import "sort"

// buildSessionAuthors is a part's session authors section: per session, every
// author (from the source key) of a source that emitted a row of it, winner or
// not, sorted and distinct. It runs at part build time over the rows before
// winner selection.
func buildSessionAuthors(members []ckpt, rows []built) map[string][]string {
	return authorsOf(rows, func(src uint16) string { return members[src].Author })
}

// authorsOf is buildSessionAuthors over any member list: author names the
// author of the source a row came from.
func authorsOf(rows []built, author func(src uint16) string) map[string][]string {
	set := map[string]map[string]bool{}
	for _, b := range rows {
		sid, a := b.row.Session, author(b.src)
		if sid == "" || a == "" {
			continue
		}
		if set[sid] == nil {
			set[sid] = map[string]bool{}
		}
		set[sid][a] = true
	}
	out := make(map[string][]string, len(set))
	for sid, authors := range set {
		out[sid] = make([]string, 0, len(authors))
		for a := range authors {
			out[sid] = append(out[sid], a)
		}
		sort.Strings(out[sid])
	}
	return out
}

// sessionAuthors is sessionID's entry in its part's session authors section,
// nil for a session db does not hold.
func (db *DB) sessionAuthors(sessionID string) []string {
	st, ok := db.part(sessionID)
	if !ok {
		return nil
	}
	return st.authOf(sessionID)
}
