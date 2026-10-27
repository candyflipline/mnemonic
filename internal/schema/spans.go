package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// SpanKind is the OTel GenAI operation a span stands for.
type SpanKind string

const (
	SpanInvokeAgent SpanKind = "invoke_agent"
	SpanChat        SpanKind = "chat"
	SpanExecuteTool SpanKind = "execute_tool"
	SpanTurn        SpanKind = "turn"
)

func SpanIDOf(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0x1f})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

var spanKinds = []SpanKind{SpanInvokeAgent, SpanChat, SpanExecuteTool, SpanTurn}

type SpanStatus string

const (
	SpanOK    SpanStatus = "ok"
	SpanError SpanStatus = "error"
)

var spanStatuses = []SpanStatus{SpanOK, SpanError}

// Span is one row of the cache's spans relation, derived from events.
// ParentSpanID is "" at the session root. Cost is nil when the source reports
// none.
type Span struct {
	SessionID        string
	SpanID           string
	ParentSpanID     string
	Kind             SpanKind
	Name             string
	Start, End       time.Time
	Status           SpanStatus
	Model            string
	InputTokens      uint64
	OutputTokens     uint64
	CacheReadTokens  uint64
	CacheWriteTokens uint64
	Cost             *float64
	FirstEventUID    string
	LastEventUID     string
}

var SpansDDL = `CREATE TABLE IF NOT EXISTS spans (
  session_id VARCHAR NOT NULL,
  span_id VARCHAR NOT NULL,
  parent_span_id VARCHAR NOT NULL,
  kind ` + enumSQL(spanKinds) + ` NOT NULL,
  name VARCHAR NOT NULL,
  start_time TIMESTAMP NOT NULL,
  end_time TIMESTAMP NOT NULL,
  status ` + enumSQL(spanStatuses) + ` NOT NULL,
  model VARCHAR NOT NULL,
  input_tokens UBIGINT NOT NULL,
  output_tokens UBIGINT NOT NULL,
  cache_read_tokens UBIGINT NOT NULL,
  cache_write_tokens UBIGINT NOT NULL,
  cost DOUBLE,
  first_event_uid VARCHAR NOT NULL,
  last_event_uid VARCHAR NOT NULL
)`

func enumSQL[T ~string](values []T) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + string(v) + "'"
	}
	return "ENUM(" + strings.Join(quoted, ", ") + ")"
}
