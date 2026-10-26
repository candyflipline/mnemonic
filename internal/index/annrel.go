package index

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// The annotations relation is the index's answer for annotations: every
// annotation-harness checkpoint's rows, grouped by target session, never in a
// part (docs/spec/spans.md). It is built at boot and rebuilt at publish only
// when the set of annotation checkpoints changed; each published DB carries
// the relation it was published with, immutable once published.

// annSet is the set of annotation checkpoints a relation was built from, the
// identity the rebuild decision reads: per source key its generation, its
// input size and its row count. The generation rides along so a re-parse
// (same size, same rows, new logs) rebuilds too.
type annSet map[string][3]int64

// annSigOf is the set cks is.
func annSigOf(cks []ckpt) annSet {
	out := make(annSet, len(cks))
	for _, c := range cks {
		out[c.Key] = [3]int64{int64(c.Gen), c.ZstSize, c.Rows}
	}
	return out
}

func (s annSet) equal(other annSet) bool {
	if len(s) != len(other) {
		return false
	}
	for k, v := range s {
		if w, ok := other[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// listAnnCkpts is every annotation-harness checkpoint of the store, in source
// key order — the order a part build offered rows in, which the relation's
// one-entry-per-annotation-id rule reads. A checkpoint that does not read
// back is not listed; the next cycle re-parses its source and rewrites it.
func listAnnCkpts(store string) ([]ckpt, error) {
	ents, err := os.ReadDir(srcDir(store))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ckpt
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".ckpt") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(srcDir(store), e.Name()))
		if err != nil {
			continue
		}
		var c ckpt
		if json.Unmarshal(b, &c) != nil {
			continue
		}
		if c.Harness == schema.AnnotationHarness {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// buildAnns builds the annotations relation of the annotation checkpoints
// cks: sources in source key order, rows in log order, the payload from the
// heavy log and the author from the row. Per target session it keeps one
// entry per annotation_id, the first row of an id, and sorts the list by ts
// then id — the part section's semantics (buildAnnotations, sortAnnotations).
func buildAnns(store string, cks []ckpt) (map[string][]schema.Annotation, error) {
	out := map[string][]schema.Annotation{}
	seen := map[string]map[string]bool{}
	for _, c := range cks {
		if c.HeavyLen == 0 {
			continue
		}
		f, err := os.Open(logPath(store, c.Key, c.Gen, "heavy"))
		if err != nil {
			return nil, err
		}
		err = walkAnnHeavy(f, c.HeavyLen, func(sid, id string, a schema.Annotation) {
			if seen[sid] == nil {
				seen[sid] = map[string]bool{}
			}
			if seen[sid][id] {
				return
			}
			seen[sid][id] = true
			a.Author = c.Author
			out[sid] = append(out[sid], a)
		})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
	}
	for _, anns := range out {
		sortAnnotations(anns)
	}
	return out, nil
}

// walkAnnHeavy hands fn each heavy record's parsed annotation in log order:
// one record per row, appended in row order, so the log is the row order.
// A line the relation cannot hold is skipped, the way buildAnnotations does.
func walkAnnHeavy(f *os.File, end int64, fn func(sid, id string, a schema.Annotation)) error {
	var scratch []byte
	for off := int64(0); off < end; {
		recs, next, err := readFrame(f, off, scratch)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			l, n := binary.Uvarint(rec)
			if n <= 0 || len(rec)-n < int(l) {
				return errShort
			}
			a, err := schema.ParseAnnotation(rec[n : n+int(l)])
			if err != nil {
				continue
			}
			fn(a.Target.SessionID, a.ID, a)
		}
		off = next
	}
	return nil
}
