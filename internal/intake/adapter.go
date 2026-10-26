package intake

// time-boxed adapter for HTTP push clients; expires 2026-12-01; delete with the PUT census (STANDING item 6)

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
)

type adapter struct {
	st  *store.Committer
	mux *http.ServeMux
}

func newAdapter(st *store.Committer) adapter {
	a := adapter{st: st, mux: http.NewServeMux()}
	a.mux.HandleFunc("GET "+wire.ManifestPath, a.manifest)
	a.mux.HandleFunc("PUT "+wire.RawPathPrefix+"{machine}/{harness}/{src_id}", a.put)
	return a
}

// ServeHTTP is GET /v1/manifest (from st.Sources, no disk read) and PUT
// /v1/raw/{machine}/{harness}/{src_id}: Applied -> 200 Source, Conflict -> 409
// Conflict{Size, HeadHash, RawEnd}.
func (a adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

func (a adapter) manifest(w http.ResponseWriter, r *http.Request) {
	if !requireAuthor(w, r) {
		return
	}
	machine := r.URL.Query().Get("machine")
	if !wire.ValidSegment(machine) {
		http.Error(w, "bad machine", http.StatusBadRequest)
		return
	}
	author := auth.Author(r.Context())
	writeJSON(w, http.StatusOK, wire.Manifest{Author: author, Machine: machine, Sources: a.st.Sources(author, machine)})
}

func (a adapter) put(w http.ResponseWriter, r *http.Request) {
	if !requireAuthor(w, r) {
		return
	}
	author := auth.Author(r.Context())
	machine, harness, srcID := r.PathValue("machine"), r.PathValue("harness"), r.PathValue("src_id")
	if err := checkSegments(machine, harness, srcID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil {
		http.Error(w, "bad offset", http.StatusBadRequest)
		return
	}
	rawEnd, err := strconv.ParseInt(q.Get("raw_end"), 10, 64)
	if err != nil {
		rawEnd = 0
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	src, op, err := parse(author, machine, harness, srcID, offset, q.Get("head_hash"), q.Get("source_name"), q.Get("path"), rawEnd, body)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errReserved) {
			code = http.StatusForbidden
		}
		http.Error(w, err.Error(), code)
		return
	}
	t, err := a.st.Submit(r.Context(), src, op)
	if err != nil {
		http.Error(w, "write", http.StatusInternalServerError)
		return
	}
	ack, err := t.Wait(r.Context())
	if err != nil {
		http.Error(w, "write", http.StatusInternalServerError)
		return
	}
	switch ack.Verdict() {
	case store.Applied:
		writeJSON(w, http.StatusOK, ack.State())
	case store.Conflict:
		s := ack.State()
		writeJSON(w, http.StatusConflict, wire.Conflict{Size: s.Size, HeadHash: s.HeadHash, RawEnd: s.RawEnd})
	default:
		http.Error(w, "write", http.StatusInternalServerError)
	}
}

func requireAuthor(w http.ResponseWriter, r *http.Request) bool {
	if auth.Author(r.Context()) == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
