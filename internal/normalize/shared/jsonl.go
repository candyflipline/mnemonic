package shared

import (
	"bufio"
	"bytes"
	"io"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/schema"
)

// ParseJSONL walks a JSONL source from the State its earlier lines ended in:
// line_no is the 1-based line index and source_offset the byte offset of the
// line start, both counted over every line including the blank and
// invalid-JSON ones that get skipped — the offset advances before any skip so
// later lines keep their true positions. Session, model and cwd hints thread
// from one line's outcome into the next and out through the returned State;
// only a reader or emit failure fails the file.
func ParseJSONL(r io.Reader, src normalize.SourceMeta, from normalize.State, s Source, emit func(*schema.Event) error) (normalize.State, error) {
	br := bufio.NewReader(r)
	sessionHint, modelHint, cwdHint := from.SessionHint, from.ModelHint, from.CwdHint
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
			Roots:        src.Roots,
		})
		sessionHint, modelHint, cwdHint = out.SessionHint, out.ModelHint, out.CwdHint
		for i := range out.Events {
			if err := emit(&out.Events[i].Event); err != nil {
				return from, err
			}
		}
	}
	return normalize.State{
		LineNo: lineNo, Offset: offset,
		SessionHint: sessionHint, ModelHint: modelHint, CwdHint: cwdHint,
		RootsDigest: src.Roots.Digest(offset),
	}, nil
}
