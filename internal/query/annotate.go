package query

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/store"
)

type annotate struct{}

func (annotate) Name() string { return "annotate" }

func init() { register(annotate{}) }

const (
	annotationNameMax  = 128
	annotationLabelMax = 128
	annotationTextMax  = 8192
	requestIDMax       = 128
)

type annotateArgs struct {
	SessionID string                  `json:"session_id"`
	SpanID    string                  `json:"span_id"`
	Name      string                  `json:"name"`
	Number    *float64                `json:"number"`
	Label     *string                 `json:"label"`
	Text      *string                 `json:"text"`
	Source    schema.AnnotationSource `json:"source"`
	RequestID string                  `json:"request_id"`
}

func parseAnnotateArgs(raw json.RawMessage) (a annotateArgs, session string, value schema.Value, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, "", value, argErrorf("invalid arguments: %s", err)
	}
	id, err := parseMcpID(a.SessionID)
	if err != nil {
		return a, "", value, err
	}
	if id.kind != kindSession {
		return a, "", value, argErrorf("session_id must be a session id")
	}
	if a.Source == "" {
		a.Source = schema.SourceHuman
	}
	switch {
	case strings.TrimSpace(a.Name) == "" || utf8.RuneCountInString(a.Name) > annotationNameMax:
		return a, "", value, argErrorf("name must be 1 to %d characters", annotationNameMax)
	case !slices.Contains([]schema.AnnotationSource{schema.SourceHuman, schema.SourceScorer, schema.SourceCI}, a.Source):
		return a, "", value, argErrorf("source must be human, scorer or ci")
	case utf8.RuneCountInString(a.RequestID) > requestIDMax:
		return a, "", value, argErrorf("request_id must be at most %d characters", requestIDMax)
	}
	set := 0
	for _, p := range []bool{a.Number != nil, a.Label != nil, a.Text != nil} {
		if p {
			set++
		}
	}
	if set != 1 {
		return a, "", value, argErrorf("give exactly one of number, label or text")
	}
	switch {
	case a.Number != nil:
		value = schema.Number(*a.Number)
	case a.Label != nil:
		if *a.Label == "" || utf8.RuneCountInString(*a.Label) > annotationLabelMax {
			return a, "", value, argErrorf("label must be 1 to %d characters", annotationLabelMax)
		}
		value = schema.Label(*a.Label)
	default:
		if *a.Text == "" || utf8.RuneCountInString(*a.Text) > annotationTextMax {
			return a, "", value, argErrorf("text must be 1 to %d characters", annotationTextMax)
		}
		value = schema.Text(*a.Text)
	}
	return a, id.session, value, nil
}

// annotationKey is the idempotency key of an annotate call: the hash of the
// annotation without its timestamp, plus the client's request_id when it gave
// one. An exact retry repeats the key; a different annotation under the same
// request_id gets its own key, so the store never mistakes it for the first.
func annotationKey(a annotateArgs, session string, value schema.Value) (string, error) {
	b, err := json.Marshal(struct {
		Target    schema.Target           `json:"target"`
		Name      string                  `json:"name"`
		Value     schema.Value            `json:"value"`
		Source    schema.AnnotationSource `json:"source"`
		RequestID string                  `json:"request_id,omitempty"`
	}{schema.Target{SessionID: session, SpanID: a.SpanID}, a.Name, value, a.Source, a.RequestID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func annotationID(author, key string) string {
	sum := sha256.Sum256([]byte(author + "\x00" + key))
	return hex.EncodeToString(sum[:16])
}

type annotateResult struct {
	AnnotationID string `json:"annotation_id"`
	SessionID    string `json:"session_id"`
	SpanID       string `json:"span_id,omitempty"`
	Name         string `json:"name"`
	RequestKey   string `json:"request_key"`
	Duplicate    bool   `json:"duplicate"`
}

func (annotate) Call(ctx context.Context, db *sql.DB, raw json.RawMessage) (any, string, error) {
	id, ok := auth.From(ctx)
	if !ok || store.From(ctx) == nil {
		return nil, "", fmt.Errorf("query: annotate needs an identity and a store on ctx")
	}
	if !id.Can(auth.Annotate) {
		return nil, "", argErrorf("token lacks scope annotate")
	}
	a, session, value, err := parseAnnotateArgs(raw)
	if err != nil {
		return nil, "", err
	}
	if err := requireTarget(ctx, db, session, a.SpanID); err != nil {
		return nil, "", err
	}
	key, err := annotationKey(a, session, value)
	if err != nil {
		return nil, "", err
	}
	note := schema.Annotation{
		ID:     annotationID(id.Author(), key),
		Target: schema.Target{SessionID: session, SpanID: a.SpanID},
		Name:   a.Name,
		Value:  value,
		Source: a.Source,
		TS:     time.Now().UTC().Truncate(time.Millisecond),
	}
	line, err := json.Marshal(note)
	if err != nil {
		return nil, "", err
	}
	_, duplicate, err := store.From(ctx).Append(store.Today(id.Author(), schema.AnnotationHarness, ""), key, append(line, '\n'))
	if err != nil {
		return nil, "", err
	}
	data := annotateResult{AnnotationID: note.ID, SessionID: a.SessionID, SpanID: a.SpanID, Name: a.Name, RequestKey: key, Duplicate: duplicate}
	text := "Annotated " + a.SessionID + " with " + a.Name + "=" + valueText(value) + "."
	if duplicate {
		text = "Already annotated: " + a.Name + "=" + valueText(value) + " on " + a.SessionID + " (no change)."
	}
	return data, text, nil
}

// requireTarget is nil when the caller may open session and, if spanID is
// set, the session has that span.
func requireTarget(ctx context.Context, db *sql.DB, session, spanID string) error {
	f, err := Sessions(ctx, db)
	if err != nil {
		return err
	}
	scope := ""
	if p := f.Opened("e.session_id"); p != "" {
		scope = " AND " + p
	}
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events e WHERE e.session_id = "+sqlQuote(session)+" AND e."+published+scope).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return argErrorf("session not found")
	}
	if spanID == "" {
		return nil
	}
	scope = ""
	if p := f.Opened("s.session_id"); p != "" {
		scope = " AND " + p
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM spans s WHERE s.session_id = "+sqlQuote(session)+" AND s.span_id = "+sqlQuote(spanID)+scope).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return argErrorf("span not found in the session")
	}
	return nil
}

func valueText(v schema.Value) string {
	if n, ok := v.Number(); ok {
		return fmt.Sprintf("%g", n)
	}
	if s, ok := v.Label(); ok {
		return s
	}
	s, _ := v.Text()
	return fmt.Sprintf("%q", s)
}
