package otlp

import (
	"cmp"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	colllogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

const noService = "unknown_service"

// batch is what one request leaves: the records to store per service, and how
// many items the request carried that were refused.
type batch struct {
	byService map[string][]Record
	rejected  int64
	reason    string
}

func newBatch() *batch { return &batch{byService: map[string][]Record{}} }

func (b *batch) reject(reason string) {
	b.rejected++
	if b.reason == "" {
		b.reason = reason
	}
}

func (b *batch) add(service string, r Record) { b.byService[service] = append(b.byService[service], r) }

func (b *batch) message() string {
	if b.rejected == 0 {
		return ""
	}
	return fmt.Sprintf("%d rejected: %s", b.rejected, b.reason)
}

func fromTraces(req *colltrace.ExportTraceServiceRequest, storeContent bool) *batch {
	b := newBatch()
	for _, rs := range req.GetResourceSpans() {
		res, service := resourceOf(rs.GetResource(), storeContent)
		for _, ss := range rs.GetScopeSpans() {
			scope := Scope{Name: ss.GetScope().GetName(), Version: ss.GetScope().GetVersion()}
			for _, sp := range ss.GetSpans() {
				traceID, spanID := idHex(sp.GetTraceId(), 16), idHex(sp.GetSpanId(), 8)
				if traceID == "" || spanID == "" {
					b.reject("span without a 16-byte trace_id and an 8-byte span_id")
					continue
				}
				r := Record{
					Signal: SignalSpan, TraceID: traceID, SpanID: spanID, ParentSpanID: idHex(sp.GetParentSpanId(), 8),
					Name: sp.GetName(), SpanKind: int32(sp.GetKind()),
					StartUnixNano: sp.GetStartTimeUnixNano(), EndUnixNano: sp.GetEndTimeUnixNano(),
					StatusCode: int32(sp.GetStatus().GetCode()), StatusMessage: sp.GetStatus().GetMessage(),
					Attributes: attrsOf(sp.GetAttributes()), Resource: res, Scope: scope, ContentStored: storeContent,
				}
				for _, ev := range sp.GetEvents() {
					se := SpanEvent{Name: ev.GetName(), TimeUnixNano: ev.GetTimeUnixNano(), Attributes: attrsOf(ev.GetAttributes())}
					if !storeContent {
						gate(se.Attributes, se.Name)
					}
					r.Events = append(r.Events, se)
				}
				if !storeContent {
					gate(r.Attributes, r.Name)
				}
				b.add(service, r)
			}
		}
	}
	stampSessions(b)
	return b
}

func fromLogs(req *colllogs.ExportLogsServiceRequest, storeContent bool) *batch {
	b := newBatch()
	for _, rl := range req.GetResourceLogs() {
		res, service := resourceOf(rl.GetResource(), storeContent)
		for _, sl := range rl.GetScopeLogs() {
			scope := Scope{Name: sl.GetScope().GetName(), Version: sl.GetScope().GetVersion()}
			for _, lr := range sl.GetLogRecords() {
				r := Record{
					Signal: SignalLog, TraceID: idHex(lr.GetTraceId(), 16), SpanID: idHex(lr.GetSpanId(), 8),
					Name: lr.GetEventName(), TimeUnixNano: lr.GetTimeUnixNano(), ObservedNano: lr.GetObservedTimeUnixNano(),
					SeverityNum: int32(lr.GetSeverityNumber()), SeverityText: lr.GetSeverityText(),
					Attributes: attrsOf(lr.GetAttributes()), Resource: res, Scope: scope, ContentStored: storeContent,
				}
				if r.Name == "" {
					r.Name = Str(r.Attributes, EventNameKey)
				}
				if storeContent {
					r.Body = anyOf(lr.GetBody())
				} else {
					gate(r.Attributes, r.Name)
				}
				b.add(service, r)
			}
		}
	}
	stampSessions(b)
	for service, rs := range b.byService {
		kept := rs[:0]
		for _, r := range rs {
			if r.SessionID == "" {
				b.reject("log record with no session.id, gen_ai.conversation.id, conversation.id or trace_id")
				continue
			}
			kept = append(kept, r)
		}
		b.byService[service] = kept
	}
	return b
}

// stampSessions gives each record its session: the first SessionKeys
// attribute it carries (record before resource), else the one a record of its
// trace in this request carries, else the trace id. Spans of one trace that
// arrive in different requests, only some naming a session, land in
// different sessions.
func stampSessions(b *batch) {
	byTrace := map[string]string{}
	for _, rs := range b.byService {
		for i := range rs {
			rs[i].SessionID = rs[i].LookupStr(SessionKeys...)
			if rs[i].SessionID != "" && rs[i].TraceID != "" {
				if _, ok := byTrace[rs[i].TraceID]; !ok {
					byTrace[rs[i].TraceID] = rs[i].SessionID
				}
			}
		}
	}
	for _, rs := range b.byService {
		for i := range rs {
			if rs[i].SessionID == "" {
				rs[i].SessionID = cmp.Or(byTrace[rs[i].TraceID], rs[i].TraceID)
			}
		}
	}
}

func resourceOf(res *resourcepb.Resource, storeContent bool) (map[string]any, string) {
	attrs := attrsOf(res.GetAttributes())
	service := Str(attrs, ServiceName)
	if !storeContent {
		gate(attrs, "")
	}
	return attrs, streamName(service)
}

// streamName is service.name as a source-name segment: lowercase letters,
// digits, dot, dash and underscore, at most 64 of them.
func streamName(service string) string {
	var sb strings.Builder
	for _, c := range strings.ToLower(service) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			sb.WriteRune(c)
		default:
			sb.WriteByte('_')
		}
		if sb.Len() == 64 {
			break
		}
	}
	s := sb.String()
	if s == "" || s == "." || s == ".." {
		return noService
	}
	return s
}

func idHex(b []byte, size int) string {
	if len(b) != size {
		return ""
	}
	for _, c := range b {
		if c != 0 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

func attrsOf(kvs []*commonpb.KeyValue) map[string]any {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = anyOf(kv.GetValue())
	}
	return m
}

func anyOf(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return json.Number(strconv.FormatInt(x.IntValue, 10))
	case *commonpb.AnyValue_DoubleValue:
		if math.IsNaN(x.DoubleValue) || math.IsInf(x.DoubleValue, 0) {
			return fmt.Sprint(x.DoubleValue)
		}
		return x.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, anyOf(e))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		return attrsOf(x.KvlistValue.GetValues())
	}
	return nil
}
