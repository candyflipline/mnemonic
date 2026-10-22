package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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

// Append adds lines, whole lines each ending in '\n', to the server-owned
// source src, creating it when missing, and wakes converge. requestKey makes
// it idempotent: a key already applied to src, or to the day before it,
// changes nothing and reports duplicate. The manifest is written last and
// records the applied keys, so a crash before it leaves the key unapplied and
// the retry truncates the torn frame and writes it again.
func (s *Store) Append(src ServerSource, requestKey string, lines []byte) (_ wire.Source, duplicate bool, _ error) {
	if err := src.validate(); err != nil {
		return wire.Source{}, false, err
	}
	if requestKey == "" {
		return wire.Source{}, false, errors.New("store: request key is required")
	}
	if len(lines) == 0 || lines[len(lines)-1] != '\n' {
		return wire.Source{}, false, errors.New("store: body must be non-empty whole lines")
	}
	prev := src
	prev.Day = src.Day.AddDate(0, 0, -1)
	if m, err := readManifest(prev.key().Manifest(s.DataDir)); err == nil && slices.Contains(m.Applied, requestKey) {
		return m.Source, true, nil
	}
	k := src.key()
	defer s.lockKey(k)()

	m, err := readManifest(k.Manifest(s.DataDir))
	switch {
	case os.IsNotExist(err):
		zst, err := writeZst(k.Raw(s.DataDir), lines)
		if err != nil {
			return wire.Source{}, false, err
		}
		first, _, _ := bytes.Cut(lines, []byte{'\n'})
		m = &manifestBody{
			Source: wire.Source{
				SrcID:      k.SrcID,
				Harness:    k.Harness,
				SourceName: src.name(),
				Path:       src.name(),
				Size:       int64(len(lines)),
				HeadHash:   wire.HeadHash(lines[:len(first)+1]),
				UpdatedMs:  time.Now().UnixMilli(),
			},
			ZstSize: zst,
		}
	case err != nil:
		return wire.Source{}, false, err
	case slices.Contains(m.Applied, requestKey):
		return m.Source, true, nil
	default:
		zst, err := appendZst(k.Raw(s.DataDir), m.ZstSize, lines)
		if err != nil {
			return wire.Source{}, false, err
		}
		m.Size += int64(len(lines))
		m.UpdatedMs = max(time.Now().UnixMilli(), m.UpdatedMs+1)
		m.ZstSize = zst
	}
	m.Applied = append(m.Applied, requestKey)
	if err := writeManifest(k.Manifest(s.DataDir), *m); err != nil {
		return wire.Source{}, false, err
	}
	s.wake()
	return m.Source, false, nil
}

type ctxKey struct{}

// WithStore puts st on ctx, the way an MCP tool reaches Append; the caller's
// author is auth.Author on the same ctx.
func WithStore(ctx context.Context, st *Store) context.Context {
	return context.WithValue(ctx, ctxKey{}, st)
}

// From is the Store WithStore put on ctx, nil when there is none.
func From(ctx context.Context) *Store {
	st, _ := ctx.Value(ctxKey{}).(*Store)
	return st
}
