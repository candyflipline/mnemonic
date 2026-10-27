package normalize

import (
	"io"

	"github.com/candyflipline/mnemonic/internal/schema"
)

type SourceMeta struct {
	SourceName string
	Harness    string
	SourceFile string
}

// State is where a parse stopped: the next line's number and byte offset and
// the hints the next line inherits. RecordTSHint is the last record timestamp
// read, which a line without one takes; lines before the first timestamp take
// that first one. The zero State is the start of a file, and parsing a file's
// tail from the State its head ended in yields the rows a whole-file parse
// gives those tail lines, unless the State is Provisional.
type State struct {
	LineNo       uint64 `json:"line_no"`
	Offset       uint64 `json:"offset"`
	SessionHint  string `json:"session_hint"`
	ModelHint    string `json:"model_hint"`
	CwdHint      string `json:"cwd_hint"`
	RecordTSHint string `json:"record_ts_hint"`
}

// Provisional reports that lines were read but none carried a record
// timestamp, so the rows emitted for them take the first timestamp of lines
// not read yet: a resumed parse cannot yield a whole-file parse's rows.
func (s State) Provisional() bool { return s.LineNo > 0 && s.RecordTSHint == "" }

type Parser interface {
	Harness() string
	Version() int
	Parse(r io.Reader, src SourceMeta, from State, emit func(*schema.Event) error) (State, error)
}

var parsers = map[string]Parser{}

func Register(p Parser) {
	if _, dup := parsers[p.Harness()]; dup {
		panic("normalize: duplicate parser for harness " + p.Harness())
	}
	parsers[p.Harness()] = p
}

func For(harness string) (Parser, bool) {
	p, ok := parsers[harness]
	return p, ok
}

func Versions() map[string]int {
	vs := make(map[string]int, len(parsers))
	for h, p := range parsers {
		vs[h] = p.Version()
	}
	return vs
}
