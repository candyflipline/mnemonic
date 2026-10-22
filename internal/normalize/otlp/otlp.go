// Package otlp parses the otlp source the receiver writes: one events row per
// stored span or log record. docs/spec/otlp.md names every column.
package otlp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"time"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/otlp"
	"github.com/candyflipline/mnemonic/internal/schema"
)

type Parser struct{}

func (Parser) Harness() string { return otlp.Harness }

func init() { normalize.Register(Parser{}) }

const statusError = 2

func (Parser) Parse(r io.Reader, src normalize.SourceMeta, from normalize.State, emit func(*schema.Event) error) (normalize.State, error) {
	br := bufio.NewReader(r)
	offset, lineNo := from.Offset, from.LineNo
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return from, err
		}
		if len(raw) == 0 {
			break
		}
		lineNo++
		start := offset
		offset += uint64(len(raw))
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		rec, err := otlp.Decode(line)
		if err != nil {
			continue
		}
		if err := emit(event(rec, line, src, lineNo, start)); err != nil {
			return from, err
		}
	}
	return normalize.State{LineNo: lineNo, Offset: offset, SessionHint: from.SessionHint, ModelHint: from.ModelHint, CwdHint: from.CwdHint}, nil
}

func event(rec *otlp.Record, line []byte, src normalize.SourceMeta, lineNo, offset uint64) *schema.Event {
	attrs := rec.Attributes
	ev := &schema.Event{
		SessionID:         rec.SessionID,
		SourceName:        src.SourceName,
		Harness:           otlp.Harness,
		InferenceProvider: rec.LookupStr(otlp.ProviderKeys...),
		SourceFile:        src.SourceFile,
		SourceInode:       1,
		SourceGeneration:  1,
		SourceLineNo:      lineNo,
		SourceOffset:      offset,
		SourceRef:         fmt.Sprintf("%s:1:%d", src.SourceFile, lineNo),
		EventKind:         "event_msg",
		PayloadType:       rec.Signal,
		TraceID:           rec.TraceID,
		RequestID:         rec.LookupStr(otlp.ResponseIDs...),
		ToolName:          rec.LookupStr(otlp.ToolNameKeys...),
		ToolCallID:        rec.LookupStr(otlp.ToolCallIDs...),
		AgentRunID:        rec.LookupStr(otlp.AgentIDKeys...),
		AgentLabel:        rec.LookupStr(otlp.AgentNameKeys...),
		Model:             shared.CanonicalizeModel(otlp.Harness, rec.LookupStr(otlp.ModelKeys...)),
		EndpointKind:      "generation",
		ContentTypes:      []string{},
		TextContent:       rec.Name,
		PayloadJSON:       string(line),
		EventVersion:      shared.EventVersion(),
	}
	if s, ok := rec.Body.(string); ok && s != "" {
		ev.TextContent = s
	}
	ev.TextContent = shared.TruncateChars(ev.TextContent, textLimit)
	ev.TextPreview = shared.TruncateChars(ev.TextContent, previewLimit)

	ev.EventTS, ev.RecordTS, ev.SessionDate = shared.UnparseableEventTS, "", "1970-01-01"
	if ns := rec.Start(); ns != 0 {
		t := time.Unix(0, int64(ns)).UTC()
		ev.EventTS, ev.RecordTS, ev.SessionDate = shared.FormatEventTS(t), t.Format("2006-01-02T15:04:05.000000Z"), t.Format(time.DateOnly)
	}

	switch rec.Signal {
	case otlp.SignalSpan:
		ev.EventUID = otlp.SpanUID(rec.TraceID, rec.SpanID)
		ev.ItemID = rec.SpanID
		if rec.ParentSpanID != "" {
			ev.OriginEventID = otlp.SpanUID(rec.TraceID, rec.ParentSpanID)
		}
		ev.OpKind = otlp.OpSpan
		if k := otlp.KindOf(rec); k != "" {
			ev.OpKind = string(k)
		}
		if rec.EndUnixNano > rec.StartUnixNano {
			ev.LatencyMs = uint32(min((rec.EndUnixNano-rec.StartUnixNano)/uint64(time.Millisecond), uint64(^uint32(0))))
		}
		ev.ActorKind = actorOf(ev.OpKind)
		if ev.OpKind == string(schema.SpanExecuteTool) && rec.StatusCode == statusError {
			ev.ToolError = 1
		}
	default:
		ev.EventUID = otlp.LogUID(line)
		ev.OpKind = otlp.OpLog
		ev.LatencyMs = otlp.Count(attrs, "duration_ms")
	}
	switch rec.StatusCode {
	case statusError:
		ev.OpStatus = "error"
	case 1:
		ev.OpStatus = "ok"
	}

	in, out := otlp.Count(attrs, otlp.InputTokenKeys...), otlp.Count(attrs, otlp.OutputTokenKeys...)
	cacheRead, cacheWrite := otlp.Count(attrs, otlp.CacheReadTokenKeys...), otlp.Count(attrs, otlp.CacheWriteTokenKeys...)
	acct := shared.NewTokenAccounting("generation").
		WithBuckets(
			shared.BucketValue{Key: "input_text", Value: uint64(in)},
			shared.BucketValue{Key: "output_text", Value: uint64(out)},
			shared.BucketValue{Key: "input_cache_read", Value: uint64(cacheRead)},
			shared.BucketValue{Key: "input_cache_write", Value: uint64(cacheWrite)},
		).
		WithLegacyScalars(uint64(in), uint64(out), uint64(cacheRead), uint64(cacheWrite))
	ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheWriteTokens = acct.InputTokens, acct.OutputTokens, acct.CacheReadTokens, acct.CacheWriteTokens
	ev.TokenUsageBuckets, ev.TokenUsageNativeUnits = acct.Buckets(), acct.NativeUnits()
	if cost, ok := otlp.Num(attrs, otlp.CostKeys...); ok {
		ev.TokenUsageNativeUnits[otlp.CostUnit] = cost
	}
	return ev
}

func actorOf(opKind string) string {
	switch schema.SpanKind(opKind) {
	case schema.SpanChat, schema.SpanInvokeAgent:
		return "assistant"
	case schema.SpanExecuteTool:
		return "tool"
	}
	return ""
}

const (
	textLimit    = 200_000
	previewLimit = 320
)
