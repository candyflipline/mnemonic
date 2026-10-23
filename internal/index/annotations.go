package index

import "github.com/candyflipline/mnemonic/internal/schema"

// buildAnnotations is a part's annotations section: one per annotation_id
// over the annotation harness rows of its sessions, the copy
// internal/cache/index_annotations.go keeps.
func buildAnnotations(rows []*Row) []schema.Annotation { panic("lane U5-annotations") }

// sessionAnnotations is the annotations targeting sessionID.
func (db *DB) sessionAnnotations(sessionID string) []schema.Annotation {
	panic("lane U5-annotations")
}
