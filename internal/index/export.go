package index

import (
	"sort"
	"time"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// ExportSession is one session as the exports read it: its winner rows in
// event order, heavy columns included, plus the annotations targeting it.
// Harness and FirstCwd are the first row's, LastEventUs the newest row's
// event_time, the sessions-table columns the dataset export orders by.
type ExportSession struct {
	ID          string
	Rows        []Row
	Payloads    []string
	TokenUsages []string
	Annotations []schema.Annotation
	Harness     string
	FirstCwd    string
	LastEventUs int64
}

// EachSession calls fn for every session the export may read, in session id
// order; hidden reports sessions the session controls hide from the export.
//
// The rows are the winner rows the part holds, minus the annotation harness's:
// its files land in the annotations relation and never in events (the copy
// 59796bc:internal/cache/index_annotations.go kept), so no export sees them
// here.
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
		if err := fn(es); err != nil {
			return err
		}
	}
	return nil
}

// ExportSession is one session by id, ok false when the index does not hold
// it. Sessions made only of annotation rows come back with no rows.
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
	return es, true, err
}

// ExportHasSession reports whether the index holds the session.
func (db *DB) ExportHasSession(id string) bool {
	_, ok := db.part(id)
	return ok
}

// ExportSpans is the session's spans relation rows, in export order (start
// time, span id).
func (db *DB) ExportSpans(id string) []schema.Span { return db.sessionSpans(id) }

// exportSession folds the part's session i into an ExportSession.
func (st *Part) exportSession(i int) (ExportSession, error) {
	s := st.session(i)
	es := ExportSession{ID: s.id}
	for d := s.start; d < s.end; d++ {
		r, err := st.row(d)
		if err != nil {
			return es, err
		}
		if r.Harness == schema.AnnotationHarness {
			continue
		}
		if us := eventTimeUs(r); us > es.LastEventUs {
			es.LastEventUs = us
		}
		payload, usage, err := st.heavyOf(r)
		if err != nil {
			return es, err
		}
		es.Rows = append(es.Rows, *r)
		es.Payloads = append(es.Payloads, payload)
		es.TokenUsages = append(es.TokenUsages, usage)
	}
	if len(es.Rows) > 0 {
		es.Harness, es.FirstCwd = es.Rows[0].Harness, es.Rows[0].Cwd
	}
	es.Annotations = st.annOf(s.id)
	return es, nil
}

// EventTime is the row's event_time, the cache's
// coalesce(try_cast(record_ts AS TIMESTAMPTZ), ingested_at) at UTC: TimeUs
// when the record_ts parsed, else the fixed stamp that stands in for
// ingested_at.
func (r Row) EventTime() time.Time {
	return spanTime(eventTimeUs(&r))
}

// ExportHidden is the set of sessions an operator export on the server's data
// dir must not emit: every deleted session, and every private one unless
// includePrivate. The operator sees every author, so tenancy plays no part.
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
