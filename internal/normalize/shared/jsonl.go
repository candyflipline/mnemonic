package shared

import (
	"bufio"
	"bytes"
	"io"
	"strings"

	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/schema"
)

// ParseJSONL walks a JSONL source from the State its earlier lines ended in:
// line_no is the 1-based line index and source_offset the byte offset of the
// line start, both counted over every line including the blank and
// invalid-JSON ones that get skipped — the offset advances before any skip so
// later lines keep their true positions. Session, model, cwd and record
// timestamp hints thread from one line's outcome into the next and out through
// the returned State; while no line has carried a timestamp, lines are held
// until one does and take it. Only a reader or emit failure fails the file.
func ParseJSONL(r io.Reader, src normalize.SourceMeta, from normalize.State, s Source, emit func(*schema.Event) error) (normalize.State, error) {
	br := bufio.NewReader(r)
	st := from
	type line struct {
		rec            JSON
		lineNo, offset uint64
	}
	var held []line
	normalizeLine := func(l line) error {
		out := NormalizeRecord(l.rec, s, RecordParams{
			SourceName:   src.SourceName,
			SourceFile:   src.SourceFile,
			SourceLineNo: l.lineNo,
			SourceOffset: l.offset,
			SessionHint:  st.SessionHint,
			ModelHint:    st.ModelHint,
			CwdHint:      st.CwdHint,
			RecordTSHint: st.RecordTSHint,
		})
		st.SessionHint, st.ModelHint, st.CwdHint = out.SessionHint, out.ModelHint, out.CwdHint
		if _, ok := ParseRecordTS(out.RecordTS); ok {
			st.RecordTSHint = out.RecordTS
		}
		for i := range out.Events {
			if err := emit(&out.Events[i].Event); err != nil {
				return err
			}
		}
		return nil
	}
	release := func() error {
		for _, l := range held {
			if err := normalizeLine(l); err != nil {
				return err
			}
		}
		held = nil
		return nil
	}
	for {
		raw, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return from, err
		}
		if len(raw) == 0 {
			break
		}
		st.LineNo++
		l := line{lineNo: st.LineNo, offset: st.Offset}
		st.Offset += uint64(len(raw))
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			continue
		}
		if l.rec, err = DecodeJSON(trimmed); err != nil {
			continue
		}
		if st.RecordTSHint == "" {
			ts := s.RecordTS(l.rec)
			if _, ok := ParseRecordTS(ts); !ok {
				held = append(held, l)
				continue
			}
			st.RecordTSHint = strings.TrimSpace(ts)
			if err := release(); err != nil {
				return from, err
			}
		}
		if err := normalizeLine(l); err != nil {
			return from, err
		}
	}
	if err := release(); err != nil {
		return from, err
	}
	return st, nil
}
