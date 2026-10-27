package index

import "sort"

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

func (db *DB) sessionAuthors(sessionID string) []string {
	st, ok := db.part(sessionID)
	if !ok {
		return nil
	}
	return st.authOf(sessionID)
}
