package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/candyflipline/mnemonic/internal/index"
	"github.com/candyflipline/mnemonic/internal/schema"
)

// Dataset is the JSONL export: sessions filtered by one annotation, capped
// per task (docs/design-open-source-at-scale.md:105, the SWE-smith cap of at
// most 3 trajectories per instance).

// AnnotationFilter is one --annotation name[=value].
type AnnotationFilter struct {
	Name     string
	Value    string
	HasValue bool
}

// DatasetRow is one JSONL line: the session, its task, its annotations, and
// its validated ATIF trajectory.
type DatasetRow struct {
	SessionID   string          `json:"session_id"`
	Harness     string          `json:"harness"`
	Task        string          `json:"task"`
	Annotations []AnnotationOut `json:"annotations"`
	ATIF        json.RawMessage `json:"atif"`
}

// AnnotationOut is one annotation of the session, its value carrying exactly
// one of number, label or text (internal/schema/annotations.go:65-69).
type AnnotationOut struct {
	Name   string          `json:"name"`
	Value  json.RawMessage `json:"value"`
	Source string          `json:"source"`
	Author string          `json:"author"`
	TS     time.Time       `json:"ts"`
}

// SessionError is a session the dataset skipped, with why.
type SessionError struct {
	SessionID string
	Err       error
}

// sessionKey is one annotated session: the harness and cwd of its first
// published event, its newest event_time, and its annotations.
type sessionKey struct {
	id, harness, task string
	lastUs            int64
	annotations       []schema.Annotation
}

// Dataset writes the sessions matching the annotation to w as JSONL, at most
// perTask per task, newest first, one session and one step at a time. A task
// is the cwd of the session's first published event. Sessions in hidden are
// left out. A session that fails validation does not count toward its task's
// cap and comes back in skipped; Dataset errs only when every matching session
// failed.
func Dataset(w io.Writer, db *index.DB, f AnnotationFilter, perTask int, hidden map[string]bool) (skipped []SessionError, err error) {
	if f.Name == "" {
		return nil, fmt.Errorf("export: --annotation needs a name")
	}
	if perTask < 1 {
		return nil, fmt.Errorf("export: --per-task must be at least 1")
	}
	var sessions []sessionKey
	err = db.EachSession(hidden, func(es index.ExportSession) error {
		// A session the annotations relation targets but events never held
		// is not among the export's sessions.
		if len(es.Rows) == 0 {
			return nil
		}
		for _, a := range es.Annotations {
			if a.Name == f.Name && (!f.HasValue || annotationValueMatches(a.Value, f)) {
				sessions = append(sessions, sessionKey{
					id: es.ID, harness: es.Harness, task: es.FirstCwd,
					lastUs: es.LastEventUs, annotations: es.Annotations,
				})
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Newest first; the session id breaks ties so the order is total.
	sort.Slice(sessions, func(i, j int) bool { return sessionNewer(sessions[i], sessions[j]) })
	tasks := map[string]int{}
	emitted := 0
	for _, s := range sessions {
		if tasks[s.task] >= perTask {
			continue
		}
		t, err := newTrajectory(db, s.id, hidden)
		if err == nil {
			err = t.validate()
		}
		if err != nil {
			skipped = append(skipped, SessionError{s.id, err})
			continue
		}
		tasks[s.task]++
		emitted++
		if err := writeRow(w, DatasetRow{
			SessionID:   s.id,
			Harness:     s.harness,
			Task:        s.task,
			Annotations: annotationOuts(s.annotations),
		}, t); err != nil {
			return skipped, err
		}
	}
	if emitted == 0 && len(skipped) > 0 {
		return skipped, fmt.Errorf("export: every matching session failed (%d)", len(skipped))
	}
	return skipped, nil
}

// writeRow writes one JSONL line: row as json.Marshal lays it out, its atif
// the trajectory written in place of the null row carries.
func writeRow(w io.Writer, row DatasetRow, t *trajectory) error {
	line, err := json.Marshal(row)
	if err != nil {
		return err
	}
	const tail = "null}"
	if !bytes.HasSuffix(line, []byte(tail)) {
		return fmt.Errorf("export: dataset row does not end in atif")
	}
	if _, err := w.Write(line[:len(line)-len(tail)]); err != nil {
		return err
	}
	if err := t.writeCompact(w); err != nil {
		return err
	}
	_, err = io.WriteString(w, "}\n")
	return err
}

func sessionNewer(a, b sessionKey) bool {
	if a.lastUs != b.lastUs {
		return a.lastUs > b.lastUs
	}
	return a.id > b.id
}

// annotationValueMatches is the SQL the annotated-sessions list kept:
// value_label = ? OR value_text = ? OR value_number = ? with the value
// parsed as a float.
func annotationValueMatches(v schema.Value, f AnnotationFilter) bool {
	if n, ok := v.Number(); ok {
		if p, err := strconv.ParseFloat(f.Value, 64); err == nil && n == p {
			return true
		}
	}
	if s, ok := v.Label(); ok && s == f.Value {
		return true
	}
	s, _ := v.Text()
	return s == f.Value
}

// annotationOuts renders the session's annotations oldest first, each ts at
// the TIMESTAMPTZ precision the annotations relation kept.
func annotationOuts(ann []schema.Annotation) []AnnotationOut {
	sorted := make([]schema.Annotation, len(ann))
	copy(sorted, ann)
	sort.Slice(sorted, func(i, j int) bool {
		ti, tj := sorted[i].TS.Truncate(time.Microsecond), sorted[j].TS.Truncate(time.Microsecond)
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return sorted[i].ID < sorted[j].ID
	})
	out := make([]AnnotationOut, 0, len(sorted))
	for _, a := range sorted {
		o := AnnotationOut{Name: a.Name, Source: string(a.Source), Author: a.Author,
			TS: a.TS.UTC().Truncate(time.Microsecond)}
		if n, ok := a.Value.Number(); ok {
			o.Value, _ = json.Marshal(map[string]float64{"number": n})
		} else if s, ok := a.Value.Label(); ok {
			o.Value, _ = json.Marshal(map[string]string{"label": s})
		} else {
			s, _ := a.Value.Text()
			o.Value, _ = json.Marshal(map[string]string{"text": s})
		}
		out = append(out, o)
	}
	return out
}
