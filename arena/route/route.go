// Package route fans a tool call out to the readers that own its parts and
// merges their answers.
package route

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/candyflipline/mnemonic/arena/keeper"
)

type Rung int

const (
	Whole Rung = iota
	Partial
)

// LocalID is the ID of the keeper's in-process reader; Local parts go only to
// it.
const LocalID = "local"

// PhaseWhole is the phase that answers a whole tool call over the request's
// parts; every other phase is one step of a scattered tool.
const PhaseWhole = "whole"

// InfraError is a scattered phase that got no answer: the router validates a
// tool's arguments before it scatters, so a phase never returns the tool's own
// error.
type InfraError struct {
	error
	Reader string
}

func (e InfraError) Unwrap() error { return e.error }

type Reader interface {
	ID() string
	Call(ctx context.Context, req CallRequest) (CallResponse, error)
}

type GrantedPart struct {
	keeper.PartRef
	URL   string
	Local bool
}

// View is one manifest with its parts granted: Sources are the manifest's
// source rows, and every URL in Parts expires at Expires.
type View struct {
	Tenant  string
	Gen     keeper.Gen
	Seq     keeper.Seq
	Parts   []GrantedPart
	Sources json.RawMessage `json:",omitempty"`
	Expires time.Time
}

type CallRequest struct {
	Tenant string
	Parts  []GrantedPart
	Tool   string
	Args   json.RawMessage
	Phase  string
	Stats  json.RawMessage
}

type CallResponse struct {
	Data  json.RawMessage
	Text  string
	Phase json.RawMessage
}

// Owner ranks readerIDs for partID by rendezvous hash, owner first.
func Owner(partID string, readerIDs []string) []string {
	type scored struct {
		id    string
		score uint64
	}
	s := make([]scored, len(readerIDs))
	for i, id := range readerIDs {
		sum := sha256.Sum256([]byte(id + "\x00" + partID))
		s[i] = scored{id, binary.BigEndian.Uint64(sum[:8])}
	}
	sort.Slice(s, func(i, j int) bool {
		if s[i].score != s[j].score {
			return s[i].score > s[j].score
		}
		return s[i].id < s[j].id
	})
	out := make([]string, len(s))
	for i := range s {
		out[i] = s[i].id
	}
	return out
}

// Do answers one tool call over v at rung.
func Do(ctx context.Context, v View, readers []Reader, rung Rung, tool string, args json.RawMessage) (json.RawMessage, string, error) {
	if answer := rules[tool].view; answer != nil {
		return answer(v, args)
	}
	switch rung {
	case Whole:
		i := slices.IndexFunc(readers, func(r Reader) bool { return r.ID() == LocalID })
		if i < 0 {
			return nil, "", errors.New("route: no local reader for the whole rung")
		}
		resp, err := readers[i].Call(ctx, CallRequest{Tenant: v.Tenant, Parts: v.Parts, Tool: tool, Args: args, Phase: PhaseWhole})
		return resp.Data, resp.Text, err
	case Partial:
		return partial(ctx, v, readers, tool, args)
	}
	return nil, "", fmt.Errorf("route: unknown rung %d", rung)
}

// scatter sends every Local part to the reader whose ID is LocalID and every
// other part to its rendezvous owner (Owner over the reader IDs), one
// CallRequest per reader carrying that reader's parts. On a reader error it
// reassigns that reader's parts to their next rank and retries once.
func scatter(ctx context.Context, v View, readers []Reader, tool, phase string, args, stats json.RawMessage) ([]CallResponse, error) {
	byID := make(map[string]Reader, len(readers))
	var remote []string
	for _, r := range readers {
		byID[r.ID()] = r
		if r.ID() != LocalID {
			remote = append(remote, r.ID())
		}
	}
	rank := make(map[string][]string, len(v.Parts))
	assign := map[string][]GrantedPart{}
	for _, p := range v.Parts {
		rk := []string{LocalID}
		if !p.Local {
			rk = Owner(p.ID, remote)
		}
		if len(rk) == 0 || byID[rk[0]] == nil {
			return nil, fmt.Errorf("route: no reader for part %s", p.ID)
		}
		rank[p.ID] = rk
		assign[rk[0]] = append(assign[rk[0]], p)
	}
	call := func(round map[string][]GrantedPart) ([]CallResponse, map[string]error) {
		ids := make([]string, 0, len(round))
		for id := range round {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out := make([]CallResponse, len(ids))
		errs := make([]error, len(ids))
		var wg sync.WaitGroup
		for i, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out[i], errs[i] = byID[id].Call(ctx, CallRequest{Tenant: v.Tenant, Parts: round[id], Tool: tool, Args: args, Phase: phase, Stats: stats})
			}()
		}
		wg.Wait()
		var ok []CallResponse
		failed := map[string]error{}
		for i, id := range ids {
			if errs[i] != nil {
				failed[id] = InfraError{error: fmt.Errorf("reader %s: %w", id, errs[i]), Reader: id}
			} else {
				ok = append(ok, out[i])
			}
		}
		return ok, failed
	}
	resps, failed := call(assign)
	if len(failed) == 0 {
		return resps, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	retry := map[string][]GrantedPart{}
	for _, id := range failedIDs(failed) {
		ferr := failed[id]
		for _, p := range assign[id] {
			rk := rank[p.ID]
			next := slices.IndexFunc(rk[slices.Index(rk, id)+1:], func(c string) bool { _, bad := failed[c]; return !bad })
			if next < 0 {
				return nil, ferr
			}
			to := rk[slices.Index(rk, id)+1+next]
			retry[to] = append(retry[to], p)
		}
	}
	more, failed := call(retry)
	if len(failed) > 0 {
		return nil, failed[failedIDs(failed)[0]]
	}
	return append(resps, more...), nil
}

// failedIDs lists the failed readers, the keeper's local reader first, so the
// error a call reports does not depend on map order.
func failedIDs(failed map[string]error) []string {
	ids := make([]string, 0, len(failed))
	for id := range failed {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == LocalID) != (ids[j] == LocalID) {
			return ids[i] == LocalID
		}
		return ids[i] < ids[j]
	})
	return ids
}

// statusError is a reader server's non-200 answer; 500 carries a tool's own
// error text.
type statusError struct {
	code int
	msg  string
}

func (e statusError) Error() string { return e.msg }

type httpReader struct {
	id, base string
	client   *http.Client
}

// HTTPReader is a Reader that calls the reader server at baseURL; a call
// that has not answered within timeout fails, so scatter fails it over.
func HTTPReader(id, baseURL string, timeout time.Duration) Reader {
	return &httpReader{id: id, base: baseURL, client: &http.Client{Timeout: timeout}}
}

func (h *httpReader) ID() string { return h.id }

func (h *httpReader) Call(ctx context.Context, req CallRequest) (CallResponse, error) {
	var resp CallResponse
	body, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/v1/call", bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	hr.Header.Set("Content-Type", "application/json")
	res, err := h.client.Do(hr)
	if err != nil {
		return resp, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return resp, statusError{code: res.StatusCode, msg: fmt.Sprintf("%s: %s", res.Status, bytes.TrimSpace(msg))}
	}
	return resp, json.NewDecoder(res.Body).Decode(&resp)
}
