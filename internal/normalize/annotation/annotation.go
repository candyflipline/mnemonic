// Package annotation parses the annotation source the annotate tool appends
// to: one events row per annotation line, as schema.AnnotationHarness names.
package annotation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/normalize/shared"
	"github.com/candyflipline/mnemonic/internal/schema"
)

type Parser struct{}

func (Parser) Harness() string { return schema.AnnotationHarness }

func init() { normalize.Register(Parser{}) }

const previewLimit = 320

func (Parser) Parse(r io.Reader, src normalize.SourceMeta, from normalize.State, emit func(*schema.Event) error) (normalize.State, error) {
	br := bufio.NewReader(r)
	offset, lineNo, recordTS := from.Offset, from.LineNo, from.RecordTSHint
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
		a, err := schema.ParseAnnotation(line)
		if err != nil {
			continue
		}
		ev, err := event(a, src, lineNo, start)
		if err != nil {
			return from, err
		}
		recordTS = ev.RecordTS
		if err := emit(ev); err != nil {
			return from, err
		}
	}
	return normalize.State{LineNo: lineNo, Offset: offset, SessionHint: from.SessionHint, ModelHint: from.ModelHint, CwdHint: from.CwdHint, RecordTSHint: recordTS}, nil
}

func event(a schema.Annotation, src normalize.SourceMeta, lineNo, offset uint64) (*schema.Event, error) {
	payload, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	t := a.TS.UTC()
	acct := shared.NewTokenAccounting("generation")
	ev := &schema.Event{
		EventUID:          a.ID,
		SessionID:         a.Target.SessionID,
		SessionDate:       t.Format(time.DateOnly),
		SourceName:        src.SourceName,
		Harness:           schema.AnnotationHarness,
		SourceFile:        src.SourceFile,
		SourceInode:       1,
		SourceGeneration:  1,
		SourceLineNo:      lineNo,
		SourceOffset:      offset,
		SourceRef:         fmt.Sprintf("%s:1:%d", src.SourceFile, lineNo),
		RecordTS:          t.Format("2006-01-02T15:04:05.000000Z"),
		EventTS:           shared.FormatEventTS(t),
		EventKind:         "event_msg",
		PayloadType:       "annotation",
		ItemID:            a.Target.SpanID,
		EndpointKind:      "generation",
		ContentTypes:      []string{},
		TextContent:       a.Name,
		TextPreview:       shared.TruncateChars(a.Name, previewLimit),
		PayloadJSON:       string(payload),
		TokenUsageBuckets: acct.Buckets(), TokenUsageNativeUnits: acct.NativeUnits(),
		EventVersion: shared.EventVersion(),
	}
	return ev, nil
}
