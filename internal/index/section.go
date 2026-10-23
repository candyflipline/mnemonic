package index

import (
	"encoding/json"
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
// current in seqs, into the three sections. Spans are derived per harness
// from that harness's rows; annotation rows carry their payload from the
// part's heavy log; the authors entry here sees winner rows only, so
// buildIndex overwrites it with buildSessionAuthors over every row.
func (s *sections) addSession(sid string, evs []built, seqs []uint32, payload func(built) string) {
	perHarness := map[string][]*Row{}
	for i, b := range evs {
		r := *b.row
		r.Author = b.row.Author
		if i < len(seqs) {
			r.TurnIndex = uint64(seqs[i])
		}
		perHarness[r.Harness] = append(perHarness[r.Harness], &r)
	}
	hs := make([]string, 0, len(perHarness))
	for h := range perHarness {
		hs = append(hs, h)
	}
	slices.Sort(hs)
	for _, h := range hs {
		if f, ok := spanBuilders[h]; ok {
			s.spans[sid] = append(s.spans[sid], f(h, perHarness[h])...)
		}
	}
	for _, b := range evs {
		if b.row.Harness != schema.AnnotationHarness {
			continue
		}
		a, err := schema.ParseAnnotation([]byte(payload(b)))
		if err != nil {
			continue
		}
		a.Author = b.row.Author
		s.ann[sid] = append(s.ann[sid], a)
	}
	set := s.auth[sid]
	if set == nil {
		set = map[string]bool{}
		s.auth[sid] = set
	}
	for _, b := range evs {
		if a := b.row.Author; a != "" {
			set[a] = true
		}
	}
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
		"annotations": mustJSON(s.ann),
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
	var ann map[string][]schema.Annotation
	var auth map[string][]string
	_ = json.Unmarshal(spansB, &spans)
	_ = json.Unmarshal(annB, &ann)
	_ = json.Unmarshal(authB, &auth)
	return spans, ann, auth
}
