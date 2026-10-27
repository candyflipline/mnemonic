package index

import "github.com/candyflipline/mnemonic/internal/schema"

type annRow struct {
	payload string
	author  string
}

// buildAnnotations keeps the first row of each annotation_id in rows: a full
// build passes them in (source key, event) order, a delta part in event order.
func buildAnnotations(rows []annRow) []schema.Annotation {
	out := make([]schema.Annotation, 0, len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		a, err := schema.ParseAnnotation([]byte(r.payload))
		if err != nil {
			continue
		}
		if seen[a.ID] {
			continue
		}
		seen[a.ID] = true
		a.Author = r.author
		out = append(out, a)
	}
	return out
}

func (db *DB) sessionAnnotations(sessionID string) []schema.Annotation {
	at, ok := db.annPart[sessionID]
	if !ok {
		return nil
	}
	var out []schema.Annotation
	seen := map[string]int{}
	for _, i := range at {
		for _, a := range db.parts[i].annOf(sessionID) {
			if j, ok := seen[a.ID]; ok {
				if a.TS.Before(out[j].TS) {
					out[j] = a
				}
				continue
			}
			seen[a.ID] = len(out)
			out = append(out, a)
		}
	}
	sortAnnotations(out)
	return out
}
