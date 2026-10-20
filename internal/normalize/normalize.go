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
