package normalize

import (
	"io"

	"github.com/candyflipline/mnemonic/internal/repo"
	"github.com/candyflipline/mnemonic/internal/schema"
)

// Version names the parsers' output. Lake files parsed under another
// Version are parsed again.
const Version = 1

type SourceMeta struct {
	SourceName string
	Harness    string
	SourceFile string
	Roots      repo.Book
}

// State is where a parse stopped: the next line's number and byte offset and
// the hints the next line inherits. The zero State is the start of a file,
// and parsing a file's tail from the State its head ended in yields the rows
// a whole-file parse gives those tail lines.
type State struct {
	LineNo      uint64 `json:"line_no"`
	Offset      uint64 `json:"offset"`
	SessionHint string `json:"session_hint"`
	ModelHint   string `json:"model_hint"`
	CwdHint     string `json:"cwd_hint"`
}

type Parser interface {
	Harness() string
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
