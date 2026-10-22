package export

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
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

// Dataset returns the sessions matching the annotation, at most perTask per
// task, newest first. A task is the cwd of the session's first published
// event. Sessions in hidden are left out. A session that fails validation does
// not count toward its task's cap and comes back in skipped; Dataset errs only
// when every matching session failed.
func Dataset(ctx context.Context, db *sql.DB, f AnnotationFilter, perTask int, hidden map[string]bool) (rows []DatasetRow, skipped []SessionError, err error) {
	if f.Name == "" {
		return nil, nil, fmt.Errorf("export: --annotation needs a name")
	}
	if perTask < 1 {
		return nil, nil, fmt.Errorf("export: --per-task must be at least 1")
	}
	sessions, err := annotatedSessions(ctx, db, f)
	if err != nil {
		return nil, nil, err
	}
	tasks := map[string]int{}
	for _, s := range sessions {
		if hidden[s.id] || tasks[s.task] >= perTask {
			continue
		}
		atif, err := MarshalATIF(ctx, db, s.id, hidden)
		if err != nil {
			skipped = append(skipped, SessionError{s.id, err})
			continue
		}
		anns, err := sessionAnnotations(ctx, db, s.id)
		if err != nil {
			return nil, nil, err
		}
		tasks[s.task]++
		rows = append(rows, DatasetRow{
			SessionID:   s.id,
			Harness:     s.harness,
			Task:        s.task,
			Annotations: anns,
			ATIF:        atif,
		})
	}
	if len(rows) == 0 && len(skipped) > 0 {
		return nil, skipped, fmt.Errorf("export: every matching session failed (%d)", len(skipped))
	}
	return rows, skipped, nil
}

type sessionKey struct {
	id, harness, task string
	last              time.Time
}

// annotatedSessions lists the matching sessions newest first, with the
// harness and cwd of each one's first published event.
func annotatedSessions(ctx context.Context, db *sql.DB, f AnnotationFilter) ([]sessionKey, error) {
	query := `SELECT target_session_id FROM annotations WHERE name = ?`
	args := []any{f.Name}
	if f.HasValue {
		query += ` AND (value_label = ? OR value_text = ?`
		args = append(args, f.Value, f.Value)
		if n, err := strconv.ParseFloat(f.Value, 64); err == nil {
			query += ` OR value_number = ?`
			args = append(args, n)
		}
		query += `)`
	}
	ids, err := queryStrings(ctx, db, query, args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	in := "session_id IN (" + placeholders(len(ids)) + ")"
	first, err := db.QueryContext(ctx, `SELECT e.session_id, e.harness, e.cwd, s.last_event_time
FROM sessions s LEFT JOIN events e
  ON e.session_id = s.session_id AND e.event_order = (SELECT min(event_order) FROM events WHERE session_id = s.session_id)
WHERE s.`+in, stringsAsAny(ids)...)
	if err != nil {
		return nil, err
	}
	defer first.Close()
	var sessions []sessionKey
	for first.Next() {
		var k sessionKey
		var harness, cwd sql.NullString
		if err := first.Scan(&k.id, &harness, &cwd, &k.last); err != nil {
			return nil, err
		}
		k.harness, k.task = harness.String, cwd.String
		sessions = append(sessions, k)
	}
	if err := first.Err(); err != nil {
		return nil, err
	}
	// Newest first; the session id breaks ties so the order is total.
	for i := 1; i < len(sessions); i++ {
		for j := i; j > 0 && sessionNewer(sessions[j], sessions[j-1]); j-- {
			sessions[j], sessions[j-1] = sessions[j-1], sessions[j]
		}
	}
	return sessions, nil
}

func sessionNewer(a, b sessionKey) bool {
	if !a.last.Equal(b.last) {
		return a.last.After(b.last)
	}
	return a.id > b.id
}

func placeholders(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ", "
		}
		s += "?"
	}
	return s
}

// sessionAnnotations reads the session's annotations oldest first.
func sessionAnnotations(ctx context.Context, db *sql.DB, sessionID string) ([]AnnotationOut, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, value_number, value_label, value_text, source, author, ts
FROM annotations WHERE target_session_id = ? ORDER BY ts, annotation_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnnotationOut
	for rows.Next() {
		var a AnnotationOut
		var number sql.NullFloat64
		var label, text, source sql.NullString
		if err := rows.Scan(&a.Name, &number, &label, &text, &source, &a.Author, &a.TS); err != nil {
			return nil, err
		}
		a.Source = source.String
		switch {
		case number.Valid:
			a.Value, _ = json.Marshal(map[string]float64{"number": number.Float64})
		case label.Valid:
			a.Value, _ = json.Marshal(map[string]string{"label": label.String})
		default:
			a.Value, _ = json.Marshal(map[string]string{"text": text.String})
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// queryStrings runs a one-string-column query.
func queryStrings(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func stringsAsAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
