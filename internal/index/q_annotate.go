package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/redact"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/store"
)

type annotate struct{}

func init() { register(annotate{}) }

func (annotate) Name() string { return "annotate" }

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

func parseAnnotateArgs(raw json.RawMessage) (redactedAnnotation, error) {
	var a annotateArgs
	var value schema.Value
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return redactedAnnotation{}, argErrorf("invalid arguments: %s", err)
	}
	id, err := parseMcpID(a.SessionID)
	if err != nil {
		return redactedAnnotation{}, err
	}
	if id.kind != kindSession {
		return redactedAnnotation{}, argErrorf("session_id must be a session id")
	}
	if a.Source == "" {
		a.Source = schema.SourceHuman
	}
	switch {
	case strings.TrimSpace(a.Name) == "" || utf8.RuneCountInString(a.Name) > annotationNameMax:
		return redactedAnnotation{}, argErrorf("name must be 1 to %d characters", annotationNameMax)
	case !slices.Contains([]schema.AnnotationSource{schema.SourceHuman, schema.SourceScorer, schema.SourceCI}, a.Source):
		return redactedAnnotation{}, argErrorf("source must be human, scorer or ci")
	case utf8.RuneCountInString(a.RequestID) > requestIDMax:
		return redactedAnnotation{}, argErrorf("request_id must be at most %d characters", requestIDMax)
	}
	set := 0
	for _, p := range []bool{a.Number != nil, a.Label != nil, a.Text != nil} {
		if p {
			set++
		}
	}
	if set != 1 {
		return redactedAnnotation{}, argErrorf("give exactly one of number, label or text")
	}
	switch {
	case a.Number != nil:
		value = schema.Number(*a.Number)
	case a.Label != nil:
		if *a.Label == "" || utf8.RuneCountInString(*a.Label) > annotationLabelMax {
			return redactedAnnotation{}, argErrorf("label must be 1 to %d characters", annotationLabelMax)
		}
		value = schema.Label(*a.Label)
	default:
		if *a.Text == "" || utf8.RuneCountInString(*a.Text) > annotationTextMax {
			return redactedAnnotation{}, argErrorf("text must be 1 to %d characters", annotationTextMax)
		}
		value = schema.Text(*a.Text)
	}
	return redactedAnnotation{
		target:     schema.Target{SessionID: id.session, SpanID: a.SpanID},
		sessionArg: a.SessionID,
		name:       redactUserText(a.Name),
		value:      redactValue(value),
		source:     schema.AnnotationSource(redactUserText(string(a.Source))),
		requestID:  a.RequestID,
	}, nil
}

type redactedAnnotation struct {
	target     schema.Target
	sessionArg string
	name       string
	value      schema.Value
	source     schema.AnnotationSource
	requestID  string
}

var annotateRed = sync.OnceValue(redact.Default)

func redactUserText(s string) string {
	return string(annotateRed().Line([]byte(s)))
}

func redactValue(v schema.Value) schema.Value {
	if s, ok := v.Label(); ok {
		return schema.Label(redactUserText(s))
	}
	if s, ok := v.Text(); ok {
		return schema.Text(redactUserText(s))
	}
	return v
}

func (r redactedAnnotation) key() (string, error) {
	b, err := json.Marshal(struct {
		Target    schema.Target           `json:"target"`
		Name      string                  `json:"name"`
		Value     schema.Value            `json:"value"`
		Source    schema.AnnotationSource `json:"source"`
		RequestID string                  `json:"request_id,omitempty"`
	}{r.target, r.name, r.value, r.source, r.requestID})
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

func (annotate) Call(ctx context.Context, db *DB, raw json.RawMessage) (any, string, error) {
	id, ok := auth.From(ctx)
	if !ok || store.From(ctx) == nil {
		return nil, "", fmt.Errorf("index: annotate needs an identity and a store on ctx")
	}
	if !id.Can(auth.Annotate) {
		return nil, "", argErrorf("token lacks scope annotate")
	}
	r, err := parseAnnotateArgs(raw)
	if err != nil {
		return nil, "", err
	}
	if err := requireTarget(ctx, db, r.target.SessionID, r.target.SpanID); err != nil {
		return nil, "", err
	}
	key, err := r.key()
	if err != nil {
		return nil, "", err
	}
	note := schema.Annotation{
		ID:     annotationID(id.Author(), key),
		Target: r.target,
		Name:   r.name,
		Value:  r.value,
		Source: r.source,
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
	data := annotateResult{AnnotationID: note.ID, SessionID: r.sessionArg, SpanID: r.target.SpanID, Name: r.name, RequestKey: key, Duplicate: duplicate}
	text := "Annotated " + r.sessionArg + " with " + r.name + "=" + valueText(r.value) + "."
	if duplicate {
		text = "Already annotated: " + r.name + "=" + valueText(r.value) + " on " + r.sessionArg + " (no change)."
	}
	return data, text, nil
}

func requireTarget(ctx context.Context, db *DB, session, spanID string) error {
	f, err := Sessions(ctx, db)
	if err != nil {
		return err
	}
	has, err := db.sessionHasEvents(session)
	if err != nil {
		return err
	}
	if !has || !f.Readable(session) {
		return argErrorf("session not found")
	}
	if spanID == "" {
		return nil
	}
	if sessionHasSpan(db, session, spanID) {
		return nil
	}
	return argErrorf("span not found in the session")
}

func sessionHasSpan(db *DB, session, spanID string) bool {
	st, ok := db.part(session)
	if !ok {
		return false
	}
	raw := st.sessionSpansRaw(session)
	if raw == nil {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return false
	}
	for dec.More() {
		var s struct{ SpanID string }
		if dec.Decode(&s) != nil {
			return false
		}
		if s.SpanID == spanID {
			return true
		}
	}
	return false
}

func (db *DB) sessionHasEvents(session string) (bool, error) {
	st, ok := db.part(session)
	if !ok {
		return false, nil
	}
	_, _, ok = st.sessionRange(session)
	return ok, nil
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
