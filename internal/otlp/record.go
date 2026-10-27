package otlp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
)

// Record is one stored line of the otlp source: a span or a log record, with
// its resource and scope, as the receiver kept it after the content gate.
// Author is the token's author, which the receiver stamps and the parser
// cannot learn from the source name.
// Times are Unix nanoseconds as decimal strings, the way OTLP JSON writes
// them. Attribute values are JSON scalars, arrays and objects; integers keep
// every digit.
type Record struct {
	Signal        string         `json:"signal"`
	SessionID     string         `json:"session_id"`
	Author        string         `json:"author"`
	TraceID       string         `json:"trace_id,omitempty"`
	SpanID        string         `json:"span_id,omitempty"`
	ParentSpanID  string         `json:"parent_span_id,omitempty"`
	Name          string         `json:"name,omitempty"`
	SpanKind      int32          `json:"span_kind,omitempty"`
	StartUnixNano uint64         `json:"start_unix_nano,string,omitempty"`
	EndUnixNano   uint64         `json:"end_unix_nano,string,omitempty"`
	StatusCode    int32          `json:"status_code,omitempty"`
	StatusMessage string         `json:"status_message,omitempty"`
	Events        []SpanEvent    `json:"events,omitempty"`
	TimeUnixNano  uint64         `json:"time_unix_nano,string,omitempty"`
	ObservedNano  uint64         `json:"observed_unix_nano,string,omitempty"`
	SeverityNum   int32          `json:"severity_number,omitempty"`
	SeverityText  string         `json:"severity_text,omitempty"`
	Body          any            `json:"body,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	Resource      map[string]any `json:"resource,omitempty"`
	Scope         Scope          `json:"scope"`
	ContentStored bool           `json:"content_stored"`
}

const (
	SignalSpan = "span"
	SignalLog  = "log"
)

type SpanEvent struct {
	Name         string         `json:"name"`
	TimeUnixNano uint64         `json:"time_unix_nano,string,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
}

type Scope struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

func Decode(line []byte) (*Record, error) {
	var r Record
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *Record) Start() uint64 {
	switch {
	case r.StartUnixNano != 0:
		return r.StartUnixNano
	case r.TimeUnixNano != 0:
		return r.TimeUnixNano
	}
	return r.ObservedNano
}

func (r *Record) LookupStr(keys ...string) string {
	for _, m := range [...]map[string]any{r.Attributes, r.Resource} {
		for _, k := range keys {
			if s := Str(m, k); s != "" {
				return s
			}
		}
	}
	return ""
}

func Str(attrs map[string]any, key string) string {
	switch v := attrs[key].(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func Num(attrs map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		var s string
		switch v := attrs[k].(type) {
		case json.Number:
			s = v.String()
		case string:
			s = v
		default:
			continue
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, true
		}
	}
	return 0, false
}

func Count(attrs map[string]any, keys ...string) uint32 {
	f, ok := Num(attrs, keys...)
	if !ok || f <= 0 {
		return 0
	}
	return uint32(min(f, math.MaxUint32))
}

const (
	OpSpan = "span"
	OpLog  = "log"
)

// SpanUID is the event_uid of a span: the same for every export of it by one
// author, in any request, so a repeat collapses to one event, and different
// for every other author, whose span of the same ids is another event.
func SpanUID(author, traceID, spanID string) string {
	sum := sha256.Sum256([]byte("otlp|span|" + author + "|" + traceID + "|" + spanID))
	return hex.EncodeToString(sum[:])
}

func LogUID(line []byte) string {
	h := sha256.New()
	h.Write([]byte("otlp|log|"))
	h.Write(line)
	return hex.EncodeToString(h.Sum(nil))
}
