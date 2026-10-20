package normalize

import (
	"io"

	"github.com/candyflipline/mnemonic/internal/schema"
)

// SourceMeta is what a parser knows about the file besides its bytes. The
// server always parses whole files, so Generation and Inode are 1.
type SourceMeta struct {
	SourceName string
	Harness    string
	SourceFile string
	Generation uint32
	Inode      uint64
}

type Parser interface {
	Harness() string
	Parse(r io.Reader, src SourceMeta) ([]schema.Event, error)
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
