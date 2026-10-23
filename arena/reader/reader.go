// Package reader answers route calls over parts: in process on the keeper, or
// over HTTP on a stateless reader.
package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/candyflipline/mnemonic/arena/route"
	"github.com/candyflipline/mnemonic/arena/trace"
)

const (
	dbEntries     = 4
	resolveFanout = 4
)

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
	parts, err := h.resolveAll(ctx, req)
	if err != nil {
		return route.CallResponse{}, err
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

// resolveAll resolves req's parts, resolveFanout at a time, in request order;
// the first error cancels the rest and is the one returned.
func (h *handler) resolveAll(ctx context.Context, req route.CallRequest) ([]*trace.Part, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	parts := make([]*trace.Part, len(req.Parts))
	sem := make(chan struct{}, resolveFanout)
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for i, p := range req.Parts {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			tp, err := h.resolve(ctx, req.Tenant, p)
			if err != nil {
				once.Do(func() { first = err; cancel() })
				return
			}
			parts[i] = tp
		}()
	}
	wg.Wait()
	if first == nil {
		first = ctx.Err()
	}
	return parts, first
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

// Serve is the one POST /v1/call handler: a CallRequest in, a CallResponse
// out, a text body on any error.
func Serve(r route.Reader) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/call", func(w http.ResponseWriter, hr *http.Request) {
		var req route.CallRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, hr.Body, maxBody)).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := r.Call(hr.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	return mux
}
