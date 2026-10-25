package index

import (
	"encoding/json"
	"iter"
	"slices"
	"sort"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// sections accumulates the three derived sections of one part at build time:
// spans per harness, annotations one per annotation id, and session authors.
// They are written with the part and read back by sessionSpans,
// sessionAnnotations and sessionAuthors.
type sections struct {
	spans map[string][]schema.Span
	ann   map[string][]schema.Annotation
	auth  map[string]map[string]bool
}

func newSections() *sections {
	return &sections{
		spans: map[string][]schema.Span{},
		ann:   map[string][]schema.Annotation{},
		auth:  map[string]map[string]bool{},
	}
}

// addSession folds one session's winner rows, in event order with turn_seq
// current, into the three sections. rows may be ranged over more than once
// and a yielded row is valid only during its yield. Spans are derived per
// harness from that harness's rows; annotation rows carry their payload from
// the part's heavy log; the authors entry here sees winner rows only, so
// buildIndex overwrites it with buildSessionAuthors over every row.
func (s *sections) addSession(sid string, rows iter.Seq[*Row], payload func(*Row) string) {
	set := s.auth[sid]
	if set == nil {
		set = map[string]bool{}
		s.auth[sid] = set
	}
	seen := map[string]bool{}
	var anns []annRow
	for r := range rows {
		seen[r.Harness] = true
		if r.Harness == schema.AnnotationHarness {
			anns = append(anns, annRow{payload: payload(r), author: r.Author})
		}
		if a := r.Author; a != "" {
			set[a] = true
		}
	}
	hs := make([]string, 0, len(seen))
	for h := range seen {
		hs = append(hs, h)
	}
	slices.Sort(hs)
	for _, h := range hs {
		if f, ok := spanBuilders[h]; ok {
			if sp := f(h, sid, ofHarness(rows, h)); len(sp) > 0 {
				s.spans[sid] = append(s.spans[sid], sp...)
			}
		}
	}
	if len(anns) > 0 {
		s.ann[sid] = buildAnnotations(anns)
	}
}

// ofHarness is the rows of one harness.
func ofHarness(rows iter.Seq[*Row], harness string) iter.Seq[*Row] {
	return func(yield func(*Row) bool) {
		for r := range rows {
			if r.Harness == harness && !yield(r) {
				return
			}
		}
	}
}

// annFile is one stored annotation: the schema's JSON with the author beside
// it, because the schema keeps Author out of its JSON (main holds it in a column).
type annFile struct {
	schema.Annotation
	Author string `json:"author"`
}

func marshalAnn(ann map[string][]schema.Annotation) []byte {
	out := make(map[string][]annFile, len(ann))
	for sid, as := range ann {
		fs := make([]annFile, len(as))
		for i, a := range as {
			fs[i] = annFile{Annotation: a, Author: a.Author}
		}
		out[sid] = fs
	}
	return mustJSON(out)
}

func unmarshalAnn(b []byte) map[string][]schema.Annotation {
	var files map[string][]annFile
	if json.Unmarshal(b, &files) != nil {
		return nil
	}
	out := make(map[string][]schema.Annotation, len(files))
	for sid, fs := range files {
		as := make([]schema.Annotation, len(fs))
		for i, f := range fs {
			as[i] = f.Annotation
			as[i].Author = f.Author
		}
		out[sid] = as
	}
	return out
}

// files encodes the three section files a part carries: spans and
// annotations grouped per session id, session authors as its map. Every part
// write must produce all three, so the reader never sees a missing file.
func (s *sections) files() map[string][]byte {
	for _, spans := range s.spans {
		sortSpans(spans)
	}
	for _, ann := range s.ann {
		sortAnnotations(ann)
	}
	authors := make(map[string][]string, len(s.auth))
	for sid, set := range s.auth {
		authors[sid] = sortedAuthors(set)
	}
	return map[string][]byte{
		"spans":       mustJSON(s.spans),
		"annotations": marshalAnn(s.ann),
		"authors":     mustJSON(authors),
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// sortSpans is the sessionSpans order: by start then span_id.
func sortSpans(spans []schema.Span) {
	sort.Slice(spans, func(i, j int) bool {
		if !spans[i].Start.Equal(spans[j].Start) {
			return spans[i].Start.Before(spans[j].Start)
		}
		return spans[i].SpanID < spans[j].SpanID
	})
}

func sortedAuthors(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// sortAnnotations is the sessionAnnotations order: by ts then id.
func sortAnnotations(ann []schema.Annotation) {
	sort.Slice(ann, func(i, j int) bool {
		if !ann[i].TS.Equal(ann[j].TS) {
			return ann[i].TS.Before(ann[j].TS)
		}
		return ann[i].ID < ann[j].ID
	})
}

// decodeSections fills a part's lazily read sections from the bytes the part
// was opened with.
func decodeSections(spansB, annB, authB []byte) (map[string][]schema.Span, map[string][]schema.Annotation, map[string][]string) {
	var spans map[string][]schema.Span
	var auth map[string][]string
	_ = json.Unmarshal(spansB, &spans)
	ann := unmarshalAnn(annB)
	_ = json.Unmarshal(authB, &auth)
	return spans, ann, auth
}
