package index

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"

	"github.com/candyflipline/mnemonic/internal/schema"
)

type ExportSession struct {
	ID          string
	Annotations []schema.Annotation
	Harness     string
	FirstCwd    string
	LastEventUs int64

	NRows int

	st *Part
	si int
}

// EachRow calls fn for each of the session's winner rows in event order, one
// st.row(d) at a time, skipping the annotation harness's rows (its files land
// in the annotations relation and never in events, so no export sees them
// here). fn's *Row borrows the part's frame cache and is valid only during its
// call; the next call reuses it. EachRow may be called any number of times.
func (es ExportSession) EachRow(fn func(*Row) error) error {
	if es.st == nil {
		return nil
	}
	s := es.st.session(es.si)
	for d := s.start; d < s.end; d++ {
		r, err := es.st.row(d)
		if err != nil {
			return err
		}
		if r.Harness == schema.AnnotationHarness {
			continue
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

func (es ExportSession) Payload(r *Row) (string, error) {
	p, _, err := es.st.heavyOf(r)
	return p, err
}

func (db *DB) EachSession(hidden map[string]bool, fn func(ExportSession) error) error {
	type ref struct {
		id string
		st *Part
		i  int
	}
	var refs []ref
	for _, st := range db.parts {
		for i := 0; i < st.nSessions; i++ {
			id := st.session(i).id
			if !hidden[id] {
				refs = append(refs, ref{id, st, i})
			}
		}
	}
	sort.Slice(refs, func(a, b int) bool { return refs[a].id < refs[b].id })
	for _, r := range refs {
		es, err := r.st.exportSession(r.i)
		if err != nil {
			return err
		}
		es.Annotations = db.sessionAnnotations(es.ID)
		if err := fn(es); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) ExportSession(id string) (ExportSession, bool, error) {
	st, ok := db.part(id)
	if !ok {
		return ExportSession{}, false, nil
	}
	i, ok := st.sessionIndex(id)
	if !ok {
		return ExportSession{}, false, nil
	}
	es, err := st.exportSession(i)
	es.Annotations = db.sessionAnnotations(id)
	return es, true, err
}

func (db *DB) ExportHasSession(id string) bool {
	_, ok := db.part(id)
	return ok
}

func (db *DB) ExportSpans(id string) []schema.Span { return db.sessionSpans(id) }

func (db *DB) EachExportSpan(id string, fn func(schema.Span) error) error {
	st, ok := db.part(id)
	if !ok {
		return nil
	}
	raw := st.sessionSpansRaw(id)
	if raw == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return nil
	}
	for dec.More() {
		var s schema.Span
		if err := dec.Decode(&s); err != nil {
			return err
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return nil
}

func (st *Part) exportSession(i int) (ExportSession, error) {
	s := st.session(i)
	es := ExportSession{ID: s.id, st: st, si: i}
	err := es.EachRow(func(r *Row) error {
		if es.NRows == 0 {
			es.Harness, es.FirstCwd = r.Harness, r.Cwd
		}
		if us := eventTimeUs(r); us > es.LastEventUs {
			es.LastEventUs = us
		}
		es.NRows++
		return nil
	})
	if err != nil {
		return es, err
	}
	return es, nil
}

// EventTime is the row's event_time, the cache's
// coalesce(try_cast(record_ts AS TIMESTAMPTZ), ingested_at) at UTC: TimeUs
// when the record_ts parsed, else the fixed stamp that stands in for
// ingested_at.
func (r Row) EventTime() time.Time {
	return spanTime(eventTimeUs(&r))
}

func (c *Controls) ExportHidden(includePrivate bool) map[string]bool {
	sets := c.sets()
	hidden := make(map[string]bool, len(sets.deleted)+len(sets.private))
	for _, id := range sets.deleted {
		hidden[id] = true
	}
	if !includePrivate {
		for _, id := range sets.private {
			hidden[id] = true
		}
	}
	return hidden
}
