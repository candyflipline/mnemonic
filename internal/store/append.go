package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/wire"
)

// ServerMachine is the machine segment of every source the server writes
// itself. A client PUT naming it is refused, so no client can write there.
const ServerMachine = "server.mnemonic"

// ServerSource names a server-owned source: the author it belongs to (always
// the caller's, from auth), the harness whose parser reads it, and a name
// such as "otlp/<service>/<yyyy-mm-dd>" or "annotation/<yyyy-mm-dd>" from
// which its src_id is derived.
type ServerSource struct {
	Author, Harness, Name string
}

func (src ServerSource) key() layout.Key {
	return layout.Key{Author: src.Author, Machine: ServerMachine, Harness: src.Harness, SrcID: wire.SrcID(src.Name)}
}

// Append adds lines, whole lines each ending in '\n', to the server-owned
// source src, creating it when missing, and wakes converge. requestKey makes
// it idempotent: a key already applied to src changes nothing and reports
// duplicate. The manifest is written last and records the applied keys, so a
// crash before it leaves the key unapplied and the retry truncates the torn
// frame and writes it again.
func (s *Store) Append(src ServerSource, requestKey string, lines []byte) (_ wire.Source, duplicate bool, _ error) {
	for _, seg := range [...]string{src.Author, src.Harness} {
		if !wire.ValidSegment(seg) {
			return wire.Source{}, false, fmt.Errorf("store: bad path segment %q", seg)
		}
	}
	if src.Name == "" || requestKey == "" {
		return wire.Source{}, false, errors.New("store: source name and request key are required")
	}
	if len(lines) == 0 || lines[len(lines)-1] != '\n' {
		return wire.Source{}, false, errors.New("store: body must be non-empty whole lines")
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
				SourceName: src.Name,
				Path:       src.Name,
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
