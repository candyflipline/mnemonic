// Package reader answers route calls over parts: in process on the keeper, or
// over HTTP on a stateless reader.
package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/candyflipline/mnemonic/arena/route"
	"github.com/candyflipline/mnemonic/arena/trace"
)

const dbEntries = 4

// handler is the one place phase semantics live. Calls are serialized because
// a Part's scratch buffer and a DB's frame cache are not safe for concurrent use.
type handler struct {
	resolve func(ctx context.Context, tenant string, p route.GrantedPart) (*trace.Part, error)
	mu      sync.Mutex
	dbs     []dbEntry
}

type dbEntry struct {
	key string
	db  *trace.DB
}

func (h *handler) call(ctx context.Context, req route.CallRequest) (route.CallResponse, error) {
	parts := make([]*trace.Part, len(req.Parts))
	for i, p := range req.Parts {
		var err error
		if parts[i], err = h.resolve(ctx, req.Tenant, p); err != nil {
			return route.CallResponse{}, err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	db, err := h.db(parts)
	if err != nil {
		return route.CallResponse{}, err
	}
	args, stats := nullIsEmpty(req.Args), nullIsEmpty(req.Stats)
	if req.Phase == route.PhaseWhole {
		data, text, err := trace.Call(db, req.Tool, args)
		return route.CallResponse{Data: data, Text: text}, err
	}
	ph, err := trace.RunPhase(db, req.Tool, req.Phase, args, stats)
	return route.CallResponse{Phase: ph}, err
}

// db returns the DB over exactly these parts, in this order, building it once
// per part set: OpenParts walks every session of every part.
func (h *handler) db(parts []*trace.Part) (*trace.DB, error) {
	var k strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&k, "%p,", p)
	}
	key := k.String()
	for _, e := range h.dbs {
		if e.key == key {
			return e.db, nil
		}
	}
	db, err := trace.OpenParts(parts)
	if err != nil {
		return nil, err
	}
	if len(h.dbs) == dbEntries {
		h.dbs = h.dbs[1:]
	}
	h.dbs = append(h.dbs, dbEntry{key, db})
	return db, nil
}

func nullIsEmpty(b json.RawMessage) json.RawMessage {
	if string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	return b
}

type local struct {
	h     *handler
	parts func() map[string]*trace.Part
}

// Local is the keeper's in-process reader, ID route.LocalID, over the parts
// parts returns at each call, keyed by part ID.
func Local(parts func() map[string]*trace.Part) route.Reader {
	l := &local{parts: parts}
	l.h = &handler{resolve: func(_ context.Context, _ string, p route.GrantedPart) (*trace.Part, error) {
		if tp, ok := parts()[p.ID]; ok {
			return tp, nil
		}
		return nil, fmt.Errorf("local reader has no part %s", p.ID)
	}}
	return l
}

func (l *local) ID() string { return route.LocalID }

func (l *local) Call(ctx context.Context, req route.CallRequest) (route.CallResponse, error) {
	return l.h.call(ctx, req)
}
