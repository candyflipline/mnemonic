package intake

// SKETCH (docs/design/uploader.md): the HTTP push protocol as an adapter onto
// the committer. EXPIRES 2026-12-01: delete this file and its Mount line once
// the PUT census shows no client on it (done predicate item 6).

import (
	"net/http"

	"github.com/candyflipline/mnemonic/internal/store"
)

type adapter struct{ st *store.Committer }

// ServeHTTP is GET /v1/manifest (from st.Sources, no disk read) and PUT
// /v1/raw/{machine}/{harness}/{src_id} (servePut's parsing, moved here
// verbatim, then st.Submit and Wait: Applied -> 200 Source, Conflict -> 409
// Conflict{Size, HeadHash, RawEnd}). The responses are today's byte for byte.
func (a adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) { panic("not implemented") }
