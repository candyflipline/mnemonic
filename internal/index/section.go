package index

import (
	"bytes"
	"encoding/json"
	"iter"
	"slices"
	"sort"
	"strings"
	"sync"

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

// secOffsets is where each session's value lies in a section file, found by
// one scan on first use. The index holds offsets only; the value itself is
// decoded from the section bytes when a session is asked for.
type secOffsets struct {
	once sync.Once
	at   map[string][2]int
}

func (o *secOffsets) of(b []byte) map[string][2]int {
	o.once.Do(func() { o.at = scanSection(b) })
	return o.at
}

// raw is session id's value as stored, a slice of b, or nil.
func (o *secOffsets) raw(b []byte, id string) []byte {
	r, ok := o.of(b)[id]
	if !ok {
		return nil
	}
	return b[r[0]:r[1]:r[1]]
}

// scanSection maps each key of the JSON object b to the byte range of its
// value, without decoding any value. A section that does not scan to its end
// yields the sessions before the break.
func scanSection(b []byte) map[string][2]int {
	at := map[string][2]int{}
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return at
	}
	i = skipSpace(b, i+1)
	for i < len(b) && b[i] != '}' {
		if b[i] != '"' {
			return at
		}
		ke := skipString(b, i)
		if ke < 0 {
			return at
		}
		key := string(b[i+1 : ke-1])
		if bytes.IndexByte(b[i+1:ke-1], '\\') >= 0 {
			if json.Unmarshal(b[i:ke], &key) != nil {
				return at
			}
		}
		i = skipSpace(b, ke)
		if i >= len(b) || b[i] != ':' {
			return at
		}
		i = skipSpace(b, i+1)
		ve := skipValue(b, i)
		if ve < 0 {
			return at
		}
		at[key] = [2]int{i, ve}
		i = skipSpace(b, ve)
		if i < len(b) && b[i] == ',' {
			i = skipSpace(b, i+1)
		}
	}
	return at
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\n' || b[i] == '\t' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString is the index after the string that opens at b[i], or -1.
func skipString(b []byte, i int) int {
	for j := i + 1; j < len(b); {
		q := bytes.IndexByte(b[j:], '"')
		if q < 0 {
			return -1
		}
		q += j
		n := 0
		for k := q - 1; k > i && b[k] == '\\'; k-- {
			n++
		}
		if n%2 == 0 {
			return q + 1
		}
		j = q + 1
	}
	return -1
}

// skipValue is the index after the JSON value that starts at b[i], or -1.
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				if j = skipString(b, j); j < 0 {
					return -1
				}
				j--
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	}
	j := i
	for j < len(b) && !strings.ContainsRune(",}] \n\t\r", rune(b[j])) {
		j++
	}
	return j
}

func decodeSpans(raw []byte) []schema.Span {
	var spans []schema.Span
	_ = json.Unmarshal(raw, &spans)
	return spans
}

func decodeAnn(raw []byte) []schema.Annotation {
	var fs []annFile
	if json.Unmarshal(raw, &fs) != nil {
		return nil
	}
	as := make([]schema.Annotation, len(fs))
	for i, f := range fs {
		as[i] = f.Annotation
		as[i].Author = f.Author
	}
	return as
}

func decodeAuthors(raw []byte) []string {
	var as []string
	_ = json.Unmarshal(raw, &as)
	return as
}
