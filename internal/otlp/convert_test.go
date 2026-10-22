package otlp

import (
	"testing"

	colltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func TestSessionOfTraceAcrossServices(t *testing.T) {
	str := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	span := func(id byte, attrs ...*commonpb.KeyValue) *tracepb.Span {
		trace, sid := make([]byte, 16), make([]byte, 8)
		trace[0], sid[0] = 1, id
		return &tracepb.Span{TraceId: trace, SpanId: sid, Name: "s", Attributes: attrs}
	}
	rs := func(service string, spans ...*tracepb.Span) *tracepb.ResourceSpans {
		return &tracepb.ResourceSpans{
			Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str(ServiceName, service)}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
		}
	}
	req := &colltrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{
		rs("zeta", span(1, str("session.id", "from-zeta"))),
		rs("alpha", span(2, str("session.id", "from-alpha"))),
		rs("mid", span(3)),
	}}
	for i := 0; i < 50; i++ {
		b := fromTraces(req, false)
		if got := b.byService["mid"][0].SessionID; got != "from-alpha" {
			t.Fatalf("run %d: session %q, want from-alpha", i, got)
		}
	}
}
