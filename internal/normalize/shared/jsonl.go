package shared

import (
	"bytes"
	"io"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/schema"
)

// ParseJSONL walks one whole JSONL source: line_no is the 1-based line index
// and source_offset the byte offset of the line start, both counted over every
// line including the blank and invalid-JSON ones that get skipped — the offset
// advances before any skip so later lines keep their true positions. Session,
// model and cwd hints thread from one line's outcome into the next and die
// with the call; only a reader failure fails the file.
func ParseJSONL(r io.Reader, src normalize.SourceMeta, s Source) ([]schema.Event, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var events []schema.Event
	var sessionHint, modelHint, cwdHint string
	rest, offset, lineNo := body, uint64(0), uint64(0)
	for len(rest) > 0 {
		var raw []byte
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			raw, rest = rest[:i+1], rest[i+1:]
		} else {
			raw, rest = rest, nil
		}
		lineNo++
		startOffset := offset
		offset += uint64(len(raw))
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			continue
		}
		rec, err := DecodeJSON(trimmed)
		if err != nil {
			continue
		}
		out := NormalizeRecord(rec, s, RecordParams{
			SourceName:   src.SourceName,
			SourceFile:   src.SourceFile,
			SourceLineNo: lineNo,
			SourceOffset: startOffset,
			SessionHint:  sessionHint,
			ModelHint:    modelHint,
			CwdHint:      cwdHint,
		})
		sessionHint, modelHint, cwdHint = out.SessionHint, out.ModelHint, out.CwdHint
		for _, row := range out.Events {
			events = append(events, row.Event)
		}
	}
	return events, nil
}
