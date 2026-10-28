package store

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const ServerMachine = "server.mnemonic"

var ServerHarnesses = [...]string{schema.AnnotationHarness, "otlp", "control"}

func isServerHarness(h string) bool { return slices.Contains(ServerHarnesses[:], h) }

type ServerSource struct {
	Author, Harness, Stream string
	Day                     time.Time
}

func Today(author, harness, stream string) ServerSource {
	return ServerSource{Author: author, Harness: harness, Stream: stream, Day: utcDay(time.Now())}
}

func utcDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func (src ServerSource) name() string {
	parts := []string{src.Harness}
	if src.Stream != "" {
		parts = append(parts, src.Stream)
	}
	return strings.Join(append(parts, src.Day.Format(time.DateOnly)), "/")
}

func (src ServerSource) key() layout.Key {
	return layout.Key{Author: src.Author, Machine: ServerMachine, Harness: src.Harness, SrcID: wire.SrcID(src.name())}
}

func (src ServerSource) validate() error {
	switch {
	case !wire.ValidSegment(src.Author):
		return fmt.Errorf("store: bad author %q", src.Author)
	case !isServerHarness(src.Harness):
		return fmt.Errorf("store: harness %q is not a server harness", src.Harness)
	case src.Day.Location() != time.UTC || !src.Day.Equal(utcDay(src.Day)):
		return fmt.Errorf("store: day %s is not a UTC midnight", src.Day)
	}
	return nil
}
