package export

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// OTLP/JSON export: one ExportTraceServiceRequest per session, built from the
// spans relation. JSON field names are lowerCamelCase, traceId and spanId are
// hex strings, 64-bit integers are decimal strings, and enums are integers
// (otlp.txt:1048-1073).

const (
	// otlpScopeName names the exporter scope in every ScopeSpans.
	otlpScopeName = "mnemonic"

	// tracePrefix keeps exported trace ids disjoint from any other digest; a
	// session's trace id is stable across exports because it hashes only the
	// session id.
	tracePrefix = "mnemonic-trace:"
)

// spanKindInt maps a spans row's kind to the OTLP SpanKind enum. execute_tool
// is INTERNAL per genai-spans.md:739-800; invoke_agent is the CLIENT variant
// per genai-agent.md:189-230. OTLP/JSON carries enums as integers
// (otlp.txt:1058-1064).
func spanKindInt(kind string) int {
	if kind == "invoke_agent" {
		return 3 // SPAN_KIND_CLIENT
	}
	return 1 // SPAN_KIND_INTERNAL
}

// statusCode maps a spans row's status to the OTLP Status enum:
// STATUS_CODE_OK = 1, STATUS_CODE_ERROR = 2.
func statusCode(status string) int {
	if status == "error" {
		return 2
	}
	return 1
}

type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource   `json:"resource"`
	ScopeSpans []otlpScopeOne `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpAttr `json:"attributes"`
}

type otlpScopeOne struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpan struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []otlpAttr `json:"attributes"`
	Status            otlpStatus `json:"status"`
}

type otlpStatus struct {
	Code int `json:"code"`
}

// otlpAttr is one KeyValue. A value holds exactly one of the oneof fields;
// intValue is a 64-bit integer, so OTLP/JSON carries it as a decimal string
// (otlp.txt:1066-1072).
type otlpAttr struct {
	Key   string     `json:"key"`
	Value otlpAnyVal `json:"value"`
}

type otlpAnyVal struct {
	StringValue string `json:"stringValue,omitempty"`
	IntValue    string `json:"intValue,omitempty"`
}

func strAttr(key, value string) otlpAttr {
	return otlpAttr{Key: key, Value: otlpAnyVal{StringValue: value}}
}

func intAttr(key string, value uint64) otlpAttr {
	return otlpAttr{Key: key, Value: otlpAnyVal{IntValue: strconv.FormatUint(value, 10)}}
}

// OTLP builds the ExportTraceServiceRequest of one session from its spans,
// ordered by start time. The trace id is derived from the session id, a
// span's id is its spans.span_id (16 hex chars, 8 bytes), and the root keeps
// no parentSpanId.
func OTLP(ctx context.Context, db *sql.DB, sessionID string) (*otlpRequest, error) {
	if ok, err := sessionExists(ctx, db, sessionID); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("export: session %q not found in the cache", sessionID)
	}
	rows, err := db.QueryContext(ctx, `SELECT span_id, parent_span_id, kind, name,
  start_time, end_time, status, model, input_tokens, output_tokens
FROM spans WHERE session_id = ? ORDER BY start_time, span_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sum := sha256.Sum256([]byte(tracePrefix + sessionID))
	traceID := hex.EncodeToString(sum[:16])

	spans := []otlpSpan{}
	for rows.Next() {
		var (
			s                      otlpSpan
			kind, status, model    string
			start, end             time.Time
			inputTokens, outTokens uint64
		)
		if err := rows.Scan(&s.SpanID, &s.ParentSpanID, &kind, &s.Name, &start, &end, &status,
			&model, &inputTokens, &outTokens); err != nil {
			return nil, err
		}
		s.TraceID = traceID
		s.Kind = spanKindInt(kind)
		s.StartTimeUnixNano = strconv.FormatInt(start.UnixNano(), 10)
		s.EndTimeUnixNano = strconv.FormatInt(end.UnixNano(), 10)
		s.Status = otlpStatus{Code: statusCode(status)}
		s.Attributes = []otlpAttr{
			strAttr("gen_ai.operation.name", kind),
			strAttr("gen_ai.conversation.id", sessionID),
		}
		if model != "" {
			s.Attributes = append(s.Attributes, strAttr("gen_ai.request.model", model))
		}
		s.Attributes = append(s.Attributes,
			intAttr("gen_ai.usage.input_tokens", inputTokens),
			intAttr("gen_ai.usage.output_tokens", outTokens))
		spans = append(spans, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &otlpRequest{ResourceSpans: []otlpResourceSpans{{
		Resource: otlpResource{Attributes: []otlpAttr{strAttr("service.name", "mnemonic")}},
		ScopeSpans: []otlpScopeOne{{
			Scope: otlpScope{Name: otlpScopeName},
			Spans: spans,
		}},
	}}}, nil
}

// sessionExists reports whether the cache knows the session.
func sessionExists(ctx context.Context, db *sql.DB, sessionID string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE session_id = ?`, sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
