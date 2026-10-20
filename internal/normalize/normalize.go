// Package normalize turns one whole raw source file into event rows. Parsers
// are ports of upstream moraine's crates/moraine-ingest-core/src/sources/*
// (read them with `git show upstream/main:<path>`); testdata/goldens pins
// their output.
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
	SourceFile string // absolute path on the client
	Generation uint32
	Inode      uint64
}

// Parser reads a whole source and returns its events in file order. Per-line
// state (session, model and cwd hints) lives inside one Parse call.
type Parser interface {
	Harness() string
	Parse(r io.Reader, src SourceMeta) ([]schema.Event, error)
}

var parsers = map[string]Parser{}

// Register adds a parser; each parser package calls it from init.
func Register(p Parser) {
	if _, dup := parsers[p.Harness()]; dup {
		panic("normalize: duplicate parser for harness " + p.Harness())
	}
	parsers[p.Harness()] = p
}

// For returns the parser for harness, or false when the harness is kept raw
// only until someone ports its parser.
func For(harness string) (Parser, bool) {
	p, ok := parsers[harness]
	return p, ok
}
