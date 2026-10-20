package store

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const (
	testMachine = "lap"
	testHarness = "claude"
	testSrcID   = "7d1e5f2a0b3c4d58"
	testPath    = "/Users/x/.claude/projects/p/session.jsonl"
)

func newServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	s := &Store{DataDir: dir}
	srv := httptest.NewServer(auth.Middleware(map[string]string{auth.Hash("tok"): "chris"})(s.Handler()))
	t.Cleanup(srv.Close)
	return srv, dir
}

func testKey() layout.Key {
	return layout.Key{Author: "chris", Machine: testMachine, Harness: testHarness, SrcID: testSrcID}
}

func rawTarget(machine, harness, srcID, offset, headHash string) string {
	return "/v1/raw/" + machine + "/" + harness + "/" + srcID + "?" + putQuery(offset, headHash)
}

func putQuery(offset, headHash string) string {
	return url.Values{
		"offset":      {offset},
		"head_hash":   {headHash},
		"source_name": {"session.jsonl"},
		"path":        {testPath},
	}.Encode()
}

func do(t *testing.T, srv *httptest.Server, method, target string, token bool, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token {
		req.Header.Set("Authorization", "Bearer tok")
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func put(t *testing.T, srv *httptest.Server, target, body string) *http.Response {
	return do(t, srv, "PUT", target, true, body)
}

func get(t *testing.T, srv *httptest.Server, target string) *http.Response {
	return do(t, srv, "GET", target, true, "")
}

func decode[T any](t *testing.T, res *http.Response) (v T) {
	t.Helper()
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// rawOf decompresses the raw file a key points at.
func rawOf(t *testing.T, dir string, k layout.Key) string {
	t.Helper()
	f, err := os.Open(k.Raw(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStoreProtocol(t *testing.T) {
	srv, dir := newServer(t)
	k := testKey()
	head := wire.HeadHash([]byte("a\n"))

	// offset 0 replaces: the raw file and the manifest come up together.
	res := put(t, srv, rawTarget(testMachine, testHarness, testSrcID, "0", head), "a\nb\n")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("replace: status %d, want 200", res.StatusCode)
	}
	if src := decode[wire.Source](t, res); src.Size != 4 {
		t.Fatalf("replace: Size = %d, want 4", src.Size)
	}
	if got := rawOf(t, dir, k); got != "a\nb\n" {
		t.Fatalf("replace: raw = %q, want %q", got, "a\nb\n")
	}

	// append from Size extends the same stream.
	res = put(t, srv, rawTarget(testMachine, testHarness, testSrcID, "4", head), "c\n")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("append: status %d, want 200", res.StatusCode)
	}
	if src := decode[wire.Source](t, res); src.Size != 6 {
		t.Fatalf("append: Size = %d, want 6", src.Size)
	}
	if got := rawOf(t, dir, k); got != "a\nb\nc\n" {
		t.Fatalf("append: raw = %q, want %q", got, "a\nb\nc\n")
	}
	// zst_size covers exactly the bytes on disk, so the next append can drop
	// any tail a crash left behind.
	var m manifestBody
	b, err := os.ReadFile(k.Manifest(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(k.Raw(dir))
	if err != nil {
		t.Fatal(err)
	}
	if m.ZstSize != info.Size() {
		t.Fatalf("zst_size = %d, raw file is %d", m.ZstSize, info.Size())
	}

	// a stale offset is a conflict naming the server's state.
	res = put(t, srv, rawTarget(testMachine, testHarness, testSrcID, "4", head), "d\n")
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale append: status %d, want 409", res.StatusCode)
	}
	if c := decode[wire.Conflict](t, res); c.Size != 6 || c.HeadHash != head {
		t.Fatalf("stale append: conflict = {%d %s}, want {6 %s}", c.Size, c.HeadHash, head)
	}

	// offset 0 replaces again.
	head2 := wire.HeadHash([]byte("z\n"))
	res = put(t, srv, rawTarget(testMachine, testHarness, testSrcID, "0", head2), "z\n")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("re-replace: status %d, want 200", res.StatusCode)
	}
	if src := decode[wire.Source](t, res); src.Size != 2 {
		t.Fatalf("re-replace: Size = %d, want 2", src.Size)
	}
	if got := rawOf(t, dir, k); got != "z\n" {
		t.Fatalf("re-replace: raw = %q, want %q", got, "z\n")
	}

	// the manifest lists the one source; an unknown machine lists none.
	res = get(t, srv, "/v1/manifest?machine="+testMachine)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("manifest: status %d, want 200", res.StatusCode)
	}
	man := decode[wire.Manifest](t, res)
	if man.Author != "chris" || man.Machine != testMachine {
		t.Fatalf("manifest: author = %q machine = %q, want chris/%s", man.Author, man.Machine, testMachine)
	}
	if len(man.Sources) != 1 {
		t.Fatalf("manifest: %d sources, want 1", len(man.Sources))
	}
	s := man.Sources[0]
	if s.SrcID != testSrcID || s.Harness != testHarness || s.SourceName != "session.jsonl" ||
		s.Path != testPath || s.Size != 2 || s.HeadHash != head2 || s.UpdatedMs <= 0 {
		t.Fatalf("manifest source = %+v", s)
	}
	res = get(t, srv, "/v1/manifest?machine=nope")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unknown machine: status %d, want 200", res.StatusCode)
	}
	other := decode[wire.Manifest](t, res)
	if other.Sources == nil || len(other.Sources) != 0 {
		t.Fatalf("unknown machine: sources = %v, want []", other.Sources)
	}
}

func TestStoreRejects(t *testing.T) {
	srv, _ := newServer(t)
	head := wire.HeadHash([]byte("a\n"))
	noHead := url.Values{"offset": {"0"}, "source_name": {"session.jsonl"}, "path": {testPath}}.Encode()

	cases := [...]struct {
		name   string
		method string
		target string
		token  bool
		body   string
		want   int
	}{
		{"uppercase harness", "PUT", rawTarget(testMachine, "A", testSrcID, "0", head), true, "a\n", 400},
		{"dotdot harness", "PUT", "/v1/raw/" + testMachine + "/..%2Fx/" + testSrcID + "?" + putQuery("0", head), true, "a\n", 400},
		{"dotdot machine", "PUT", "/v1/raw/" + testMachine + "/../" + testSrcID + "?" + putQuery("0", head), true, "a\n", 404},
		{"bad offset", "PUT", rawTarget(testMachine, testHarness, testSrcID, "x", head), true, "a\n", 400},
		{"negative offset", "PUT", rawTarget(testMachine, testHarness, testSrcID, "-1", head), true, "a\n", 400},
		{"missing head_hash", "PUT", "/v1/raw/" + testMachine + "/" + testHarness + "/" + testSrcID + "?" + noHead, true, "a\n", 400},
		{"empty body", "PUT", rawTarget(testMachine, testHarness, testSrcID, "0", head), true, "", 400},
		{"no final newline", "PUT", rawTarget(testMachine, testHarness, testSrcID, "0", head), true, "a", 400},
		{"manifest bad machine", "GET", "/v1/manifest?machine=LAP", true, "", 400},
		{"manifest missing machine", "GET", "/v1/manifest", true, "", 400},
		{"put no token", "PUT", rawTarget(testMachine, testHarness, testSrcID, "0", head), false, "a\n", 401},
		{"manifest no token", "GET", "/v1/manifest?machine=" + testMachine, false, "", 401},
		{"get on raw path", "GET", "/v1/raw/a/b/c", true, "", 405},
		{"put on manifest", "PUT", "/v1/manifest", true, "a\n", 405},
		{"unknown path", "GET", "/v1/other", true, "", 404},
	}
	for _, c := range cases {
		res := do(t, srv, c.method, c.target, c.token, c.body)
		if res.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
}
