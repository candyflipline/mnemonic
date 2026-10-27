package index

import "github.com/candyflipline/mnemonic/internal/schema"

type annRow struct {
	payload string
	author  string
}

// buildAnnotations is a part's annotations section: one per annotation_id
// over the annotation harness rows of its sessions, the copy
// 59796bc:internal/cache/index_annotations.go kept. Rows arrive in (source
// key, event) order, so the first row of an id is the one kept, the way the
// oracle's applyAnnotationBatch qualifies row_number over filename and
// file_row_number.
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
