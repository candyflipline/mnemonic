package query_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/normalize"
	"github.com/candyflipline/mnemonic/internal/server"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const (
	claudeSession = "session:N2M2NjZjMDEtZDM4ZS00NjU4LTg2NTAtODU0ZmZiNWI2MjZl"
	chatSpan      = "93d47cb9e47a4461"
)

const wantTrace = `{"session_id":"session:N2M2NjZjMDEtZDM4ZS00NjU4LTg2NTAtODU0ZmZiNWI2MjZl",
"totals":{"spans":4,"tokens":{"input":9,"output":5,"cache_read":2,"cache_write":19630},"cost":null},
"tree":[{"span_id":"5c1265bbc73a071b","kind":"invoke_agent","name":"claude-code","start":"2026-01-19T15:58:40.000Z","end":"2026-01-19T15:58:43.000Z","status":"ok","tokens":{"input":9,"output":5,"cache_read":2,"cache_write":19630},"cost":null,"children":[
 {"span_id":"055bfbac1f915da7","kind":"turn","name":"turn 1","start":"2026-01-19T15:58:40.000Z","end":"2026-01-19T15:58:43.000Z","status":"ok","tokens":{"input":9,"output":5,"cache_read":2,"cache_write":19630},"cost":null,"children":[
  {"span_id":"93d47cb9e47a4461","kind":"chat","name":"claude-opus-4-5-20251101","start":"2026-01-19T15:58:41.421Z","end":"2026-01-19T15:58:41.421Z","status":"ok","model":"claude-opus-4-5-20251101","tokens":{"input":9,"output":5,"cache_read":2,"cache_write":19630},"cost":null,"children":[
   {"span_id":"712db1ba0f646f0e","kind":"execute_tool","name":"Read","start":"2026-01-19T15:58:41.421Z","end":"2026-01-19T15:58:42.000Z","status":"ok","tokens":{"input":0,"output":0,"cache_read":0,"cache_write":0},"cost":null,"children":[]}]}]}]}]}`

const wantAnnotations = `[{"id":"ID1","span_id":"93d47cb9e47a4461","name":"correctness","value":{"number":0.75},"source":"scorer","author":"alice","ts":"T"}]`

func compact(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestTraceAnnotateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tokens := auth.NewTokens(map[string]auth.Token{
		auth.Hash("alice"):  {Tenant: "a", Author: "alice", Scopes: []auth.Scope{auth.Read, auth.Push, auth.Annotate}},
		auth.Hash("reader"): {Tenant: "a", Author: "reader"},
		auth.Hash("bob"):    {Tenant: "b", Author: "bob", Scopes: []auth.Scope{auth.Read, auth.Push, auth.Annotate}},
	})
	start := func() (*server.Server, *httptest.Server) {
		srv, err := server.New(dir, tokens)
		if err != nil {
			t.Fatal(err)
		}
		return srv, httptest.NewServer(srv.Handler())
	}
	srv, ts := start()

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "claude-code", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := bytes.Cut(raw, []byte{'\n'})
	path := "/fixtures/claude-code/session.jsonl"
	q := url.Values{"offset": {"0"}, "head_hash": {wire.HeadHash(append(first, '\n'))}, "source_name": {"golden-claude-code"}, "path": {path}}
	put, _ := http.NewRequest("PUT", ts.URL+wire.RawPathPrefix+"m1/claude-code/"+wire.SrcID(path)+"?"+q.Encode(), bytes.NewReader(raw))
	put.Header.Set("Authorization", "Bearer alice")
	if res, err := http.DefaultClient.Do(put); err != nil || res.StatusCode != 200 {
		t.Fatalf("put: %v %v", res, err)
	}

	call := func(token, tool, args string) (data string, errMsg string) {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		var r struct {
			Result struct {
				IsError           bool `json:"isError"`
				StructuredContent struct {
					Data  json.RawMessage `json:"data"`
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		if r.Result.IsError {
			return "", r.Result.StructuredContent.Error.Message
		}
		return string(r.Result.StructuredContent.Data), ""
	}
	trace := func(include string) string {
		t.Helper()
		data, errMsg := call("alice", "get_trace", `{"session_id":"`+claudeSession+`"`+include+`}`)
		if errMsg != "" {
			t.Fatal(errMsg)
		}
		return data
	}
	cycle := func() {
		t.Helper()
		if err := srv.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	cycle()

	if got := trace(""); compact(t, got) != compact(t, wantTrace) {
		t.Fatalf("get_trace:\n%s\nwant:\n%s", got, compact(t, wantTrace))
	}

	args := `{"session_id":"` + claudeSession + `","span_id":"` + chatSpan + `","name":"correctness","number":0.75,"source":"scorer","request_id":"r1"}`
	if _, msg := call("reader", "annotate", args); msg != "token lacks scope annotate" {
		t.Fatalf("annotate without the scope: %q", msg)
	}
	if _, msg := call("bob", "annotate", args); msg != "session not found" {
		t.Fatalf("annotate another tenant's session: %q", msg)
	}
	if _, msg := call("alice", "annotate", `{"session_id":"`+claudeSession+`","span_id":"ffffffffffffffff","name":"x","label":"y"}`); msg != "span not found in the session" {
		t.Fatalf("annotate a span the session lacks: %q", msg)
	}
	if _, msg := call("bob", "get_trace", `{"session_id":"`+claudeSession+`"}`); msg != "session not found" {
		t.Fatalf("get_trace of another tenant's session: %q", msg)
	}
	const wantID = "326b831e21317c1de7f925b2d199f764"
	const wantKey = "9d1f735ba24066cb31c2010adf442c9ff68d334bc2f5c3183c946bb33d401f65"
	for i, wantDup := range []string{"false", "true"} {
		data, msg := call("alice", "annotate", args)
		want := `{"annotation_id":"` + wantID + `","session_id":"` + claudeSession + `","span_id":"` + chatSpan + `","name":"correctness","request_key":"` + wantKey + `","duplicate":` + wantDup + `}`
		if msg != "" || data != want {
			t.Fatalf("annotate %d: %s %q, want %s", i, data, msg, want)
		}
	}

	ts.Close()
	cycle()
	ts = httptest.NewServer(srv.Handler())
	notes := regexp.MustCompile(`"annotations":(\[.*?\]),"children"`)
	tsRe := regexp.MustCompile(`"ts":"[^"]*"`)
	wantNotes := compact(t, regexp.MustCompile("ID1").ReplaceAllString(wantAnnotations, wantID))
	check := func(label string) {
		t.Helper()
		got := trace("")
		m := notes.FindStringSubmatch(got)
		if m == nil || tsRe.ReplaceAllString(m[1], `"ts":"T"`) != wantNotes {
			t.Fatalf("%s: get_trace annotations:\n%s\nwant %s", label, got, wantNotes)
		}
		if bare := trace(`,"include_annotations":false`); compact(t, bare) != compact(t, wantTrace) {
			t.Fatalf("%s: include_annotations false:\n%s", label, bare)
		}
	}
	check("after annotate")

	ts.Close()
	srv.Close()
	for _, p := range []string{filepath.Join(dir, "events"), layout.CacheDir(dir)} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	srv, ts = start()
	defer ts.Close()
	defer srv.Close()
	cycle()
	want, _ := json.Marshal(normalize.Versions())
	if b, err := os.ReadFile(layout.ParserMarker(dir)); err != nil || string(b) != string(want)+"\n" {
		t.Fatalf("parser marker after the sweep: %q %v", b, err)
	}
	check("after the sweep")

	reused := `{"session_id":"` + claudeSession + `","span_id":"` + chatSpan + `","name":"correctness","number":0.9,"source":"scorer","request_id":"r1"}`
	data, msg := call("alice", "annotate", reused)
	if msg != "" || !strings.Contains(data, `"duplicate":false`) || strings.Contains(data, wantID) {
		t.Fatalf("a different annotation under a reused request_id: %s %q, want a new annotation, not a duplicate", data, msg)
	}
	cycle()
	if got := strings.Count(trace(""), `"name":"correctness"`); got != 2 {
		t.Fatalf("annotations after the reused request_id = %d, want 2", got)
	}
}
