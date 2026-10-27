package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// AnnotationHarness is the harness of the server-owned sources annotations
// are appended to. Its lake files load into the cache's annotations relation
// and never into events.
//
// The row contract for its parser: each raw line is one annotation, and the
// parser emits one event per line with event_uid = Annotation.ID,
// session_id = Annotation.Target.SessionID, and payload_json = the
// Annotation's JSON (what json.Marshal of it writes). The cache reads only
// event_uid, payload_json and the author the lake stamps from the source's
// path; the author in the relation is never taken from the payload.
const AnnotationHarness = "annotation"

type AnnotationSource string

const (
	SourceHuman  AnnotationSource = "human"
	SourceScorer AnnotationSource = "scorer"
	SourceCI     AnnotationSource = "ci"
)

var annotationSources = []AnnotationSource{SourceHuman, SourceScorer, SourceCI}

type Target struct {
	SessionID string `json:"session_id"`
	SpanID    string `json:"span_id,omitempty"`
}

type valueKind uint8

const (
	valueNumber valueKind = iota + 1
	valueLabel
	valueText
)

type Value struct {
	kind   valueKind
	number float64
	str    string
}

func Number(f float64) Value { return Value{kind: valueNumber, number: f} }
func Label(s string) Value   { return Value{kind: valueLabel, str: s} }
func Text(s string) Value    { return Value{kind: valueText, str: s} }

func (v Value) Number() (float64, bool) { return v.number, v.kind == valueNumber }
func (v Value) Label() (string, bool)   { return v.str, v.kind == valueLabel }
func (v Value) Text() (string, bool)    { return v.str, v.kind == valueText }

type valueJSON struct {
	Number *float64 `json:"number,omitempty"`
	Label  *string  `json:"label,omitempty"`
	Text   *string  `json:"text,omitempty"`
}

func (v Value) MarshalJSON() ([]byte, error) {
	switch v.kind {
	case valueNumber:
		return json.Marshal(valueJSON{Number: &v.number})
	case valueLabel:
		return json.Marshal(valueJSON{Label: &v.str})
	case valueText:
		return json.Marshal(valueJSON{Text: &v.str})
	}
	return nil, errors.New("schema: empty annotation value")
}

func (v *Value) UnmarshalJSON(b []byte) error {
	var j valueJSON
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&j); err != nil {
		return fmt.Errorf("schema: annotation value: %w", err)
	}
	set := 0
	for _, p := range []bool{j.Number != nil, j.Label != nil, j.Text != nil} {
		if p {
			set++
		}
	}
	if set != 1 {
		return errors.New("schema: annotation value must have exactly one of number, label, text")
	}
	switch {
	case j.Number != nil:
		*v = Number(*j.Number)
	case j.Label != nil:
		*v = Label(*j.Label)
	default:
		*v = Text(*j.Text)
	}
	return nil
}

type Annotation struct {
	ID     string           `json:"id"`
	Target Target           `json:"target"`
	Name   string           `json:"name"`
	Value  Value            `json:"value"`
	Source AnnotationSource `json:"source"`
	TS     time.Time        `json:"ts"`
	Author string           `json:"-"`
}

func ParseAnnotation(line []byte) (Annotation, error) {
	var a Annotation
	if err := json.Unmarshal(line, &a); err != nil {
		return Annotation{}, err
	}
	switch {
	case a.ID == "":
		return Annotation{}, errors.New("schema: annotation id is required")
	case a.Target.SessionID == "":
		return Annotation{}, errors.New("schema: annotation target.session_id is required")
	case a.Name == "":
		return Annotation{}, errors.New("schema: annotation name is required")
	case a.Value.kind == 0:
		return Annotation{}, errors.New("schema: annotation value is required")
	case !slices.Contains(annotationSources, a.Source):
		return Annotation{}, fmt.Errorf("schema: annotation source %q is not one of human, scorer, ci", a.Source)
	case a.TS.IsZero():
		return Annotation{}, errors.New("schema: annotation ts is required")
	}
	return a, nil
}

var AnnotationsDDL = `CREATE TABLE IF NOT EXISTS annotations (
  annotation_id VARCHAR NOT NULL,
  target_session_id VARCHAR NOT NULL,
  target_span_id VARCHAR,
  name VARCHAR NOT NULL,
  value_number DOUBLE,
  value_label VARCHAR,
  value_text VARCHAR,
  source ` + enumSQL(annotationSources) + ` NOT NULL,
  author VARCHAR NOT NULL,
  ts TIMESTAMP NOT NULL,
  lake_file VARCHAR NOT NULL,
  lake_row UBIGINT NOT NULL
)`
