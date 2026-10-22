package schema

import (
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

var spanKinds = []SpanKind{SpanInvokeAgent, SpanChat, SpanExecuteTool, SpanTurn}

type SpanStatus string

const (
	SpanOK    SpanStatus = "ok"
	SpanError SpanStatus = "error"
)

var spanStatuses = []SpanStatus{SpanOK, SpanError}

// Span is one row of the cache's spans relation, derived from events. SpanID
// is the first 16 hex chars of sha256 over the source span key, so it is the
// same after every re-parse; ParentSpanID is "" at the session root. Cost is
// nil when the source reports none.
type Span struct {
	Tenant           string
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

var SpansDDL = `CREATE TABLE spans (
  tenant VARCHAR NOT NULL,
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
