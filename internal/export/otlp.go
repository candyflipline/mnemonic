package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"

	"github.com/candyflipline/mnemonic/internal/index"
	"github.com/candyflipline/mnemonic/internal/schema"
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

// WriteOTLP writes the ExportTraceServiceRequest of one session to w, indented
// by two spaces in field order, one span at a time, the spans ordered by start
// time. The trace id is derived from the session id, a span's id is its
// spans.span_id (16 hex chars, 8 bytes), and the root keeps no parentSpanId.
// Only chat spans carry gen_ai.usage.*: the spans relation rolls usage up onto
// invoke_agent and turn spans, so a sum over the trace would count it twice. A
// session in hidden is not found.
func WriteOTLP(w io.Writer, db *index.DB, sessionID string, hidden map[string]bool) error {
	if hidden[sessionID] || !db.ExportHasSession(sessionID) {
		return notFound(sessionID)
	}
	sum := sha256.Sum256([]byte(tracePrefix + sessionID))
	traceID := hex.EncodeToString(sum[:16])
	env, err := json.MarshalIndent(otlpRequest{ResourceSpans: []otlpResourceSpans{{
		Resource: otlpResource{Attributes: []otlpAttr{strAttr("service.name", "mnemonic")}},
		ScopeSpans: []otlpScopeOne{{
			Scope: otlpScope{Name: otlpScopeName},
			Spans: []otlpSpan{},
		}},
	}}}, "", "  ")
	if err != nil {
		return err
	}
	return writeArray(w, env, `"spans": []`, "  ", func(prefix string, put func([]byte) error) error {
		for _, row := range db.ExportSpans(sessionID) {
			b, err := json.MarshalIndent(otlpSpanOf(traceID, sessionID, row), prefix, "  ")
			if err != nil {
				return err
			}
			if err := put(b); err != nil {
				return err
			}
		}
		return nil
	})
}

func otlpSpanOf(traceID, sessionID string, row schema.Span) otlpSpan {
	kind := string(row.Kind)
	s := otlpSpan{
		TraceID:           traceID,
		SpanID:            row.SpanID,
		ParentSpanID:      row.ParentSpanID,
		Name:              row.Name,
		Kind:              spanKindInt(kind),
		StartTimeUnixNano: strconv.FormatInt(row.Start.UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(row.End.UnixNano(), 10),
		Status:            otlpStatus{Code: statusCode(string(row.Status))},
		Attributes: []otlpAttr{
			strAttr("gen_ai.operation.name", kind),
			strAttr("gen_ai.conversation.id", sessionID),
		},
	}
	if row.Model != "" {
		s.Attributes = append(s.Attributes, strAttr("gen_ai.request.model", row.Model))
	}
	if kind == "chat" {
		s.Attributes = append(s.Attributes,
			intAttr("gen_ai.usage.input_tokens", row.InputTokens),
			intAttr("gen_ai.usage.output_tokens", row.OutputTokens))
	}
	return s
}
