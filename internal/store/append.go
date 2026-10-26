package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
)

// ServerMachine is the machine segment of every source the server writes
// itself. A client PUT naming it is refused, so no client can write there.
const ServerMachine = "server.mnemonic"

// ServerHarnesses are the harnesses only the server writes: a client PUT
// naming one is refused, and Append takes no other.
var ServerHarnesses = [...]string{schema.AnnotationHarness, "otlp", "control"}

func isServerHarness(h string) bool { return slices.Contains(ServerHarnesses[:], h) }

// ServerSource names a server-owned source: the author it belongs to (always
// the caller's, from auth), one of ServerHarnesses, a stream within it (an
// OTLP service, or "" for one stream per harness) and the UTC day it collects,
// a midnight in UTC. Each day is its own source, so a manifest holds one
// day's request keys.
type ServerSource struct {
	Author, Harness, Stream string
	Day                     time.Time
}

// Today is the ServerSource of author, harness and stream for the current
// UTC day.
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

type ctxKey struct{}

// WithStore puts st on ctx, the way an MCP tool reaches Append; the caller's
// author is auth.Author on the same ctx.
func WithStore(ctx context.Context, st *Committer) context.Context {
	return context.WithValue(ctx, ctxKey{}, st)
}

// From is the Committer WithStore put on ctx, nil when there is none.
func From(ctx context.Context) *Committer {
	st, _ := ctx.Value(ctxKey{}).(*Committer)
	return st
}
