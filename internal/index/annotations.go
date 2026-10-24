package index

import "github.com/candyflipline/mnemonic/internal/schema"

// annRow is one annotation harness row with its heavy columns: the payload
// the annotations relation reads its value, source and ts from, and the
// author of the source the row was parsed from.
type annRow struct {
	row     *Row
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

// sessionAnnotations is the annotations targeting sessionID.
func (db *DB) sessionAnnotations(sessionID string) []schema.Annotation {
	st, ok := db.part(sessionID)
	if !ok {
		return nil
	}
	return st.annSection()[sessionID]
}
