package push

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/wire"
)

// identityRedactor stands in for *redact.Redactor, whose rules live in
// another lane; these tests only need Line to keep bytes where they are.
type identityRedactor struct{}

func (identityRedactor) Line(line []byte) []byte { return line }

// fakeStore is the server side of the wire protocol, in memory: offset 0
// replaces, offset == size appends, anything else is a 409. The stale map lets
// a test serve a manifest whose sizes lag the store, as they do when another
// writer lands between our manifest snapshot and our PUT.
type fakeStore struct {
	srv       *httptest.Server
	mu        sync.Mutex
	sources   map[string]*fakeSource
	stale     map[string]int64
	puts      []fakePut
	auth      string
	machine   string
	updatedMs int64
}

type fakeSource struct {
	raw     []byte
	head    string
	harness string
	srcName string
	path    string
}

type fakePut struct {
	srcID      string
	offset     int64
	body, head string
	conflicted bool
}

func newTestServer(t *testing.T) *fakeStore {
	t.Helper()
	s := &fakeStore{sources: map[string]*fakeSource{}, stale: map[string]int64{}, updatedMs: 1}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/manifest", s.serveManifest)
	mux.HandleFunc("PUT /v1/raw/{machine}/{harness}/{srcid}", s.servePut)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *fakeStore) serveManifest(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = r.Header.Get("Authorization")
	s.machine = r.URL.Query().Get("machine")
	var man wire.Manifest
	for id, src := range s.sources {
		size := int64(len(src.raw))
		if stale, ok := s.stale[id]; ok {
			size = stale
		}
		man.Sources = append(man.Sources, wire.Source{
			SrcID: id, Harness: src.harness, SourceName: src.srcName, Path: src.path,
			Size: size, HeadHash: src.head, UpdatedMs: s.updatedMs,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(man)
}

func (s *fakeStore) servePut(w http.ResponseWriter, r *http.Request) {
	srcID := r.PathValue("srcid")
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		http.Error(w, "bad offset", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	head := r.URL.Query().Get("head_hash")
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[srcID]
	if src == nil {
		src = &fakeSource{}
		s.sources[srcID] = src
	}
	s.puts = append(s.puts, fakePut{
		srcID: srcID, offset: offset, body: string(body), head: head,
		conflicted: offset != 0 && offset != int64(len(src.raw)),
	})
	switch {
	case offset == 0:
		src.raw = body
		src.head = head
		src.harness = r.PathValue("harness")
		src.srcName = r.URL.Query().Get("source_name")
		src.path = r.URL.Query().Get("path")
	case offset == int64(len(src.raw)):
		src.raw = append(src.raw, body...)
		src.head = head
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(wire.Conflict{Size: int64(len(src.raw)), HeadHash: src.head})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wire.Source{
		SrcID: srcID, Harness: src.harness, SourceName: src.srcName, Path: src.path,
		Size: int64(len(src.raw)), HeadHash: src.head, UpdatedMs: 2,
	})
}

// appendDirect lands bytes on the server outside any Run, the way a second
// writer would.
func (s *fakeStore) appendDirect(t *testing.T, path, chunk string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources[wire.SrcID(path)].raw = append(s.sources[wire.SrcID(path)].raw, chunk...)
}

func (s *fakeStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.puts)
}

func (s *fakeStore) lastPut() fakePut {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts[len(s.puts)-1]
}

func testConfig(server, dir string) config.Client {
	return config.Client{
		Server:  server,
		Token:   "test-token",
		Machine: "testbox",
		Sources: []config.SourceGlob{
			{Harness: "claude-code", SourceName: "claude", Glob: filepath.Join(dir, "**", "*.jsonl")},
		},
	}
}

func runPass(t *testing.T, s *fakeStore, dir string) Stats {
	t.Helper()
	st, err := Run(context.Background(), testConfig(s.srv.URL, dir), identityRedactor{}, s.srv.Client())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return st
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertRaw(t *testing.T, s *fakeStore, path, want string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[wire.SrcID(path)]
	if src == nil {
		t.Fatalf("server has no entry for %s", path)
	}
	if string(src.raw) != want {
		t.Fatalf("server holds %q, want %q", src.raw, want)
	}
}

// Two files of whole lines upload exactly their bytes on the first pass.
func TestFirstPassUploadsWholeFiles(t *testing.T) {
	dir := t.TempDir()
	p1 := writeFile(t, dir, "a.jsonl", "alpha one\nalpha two\n") // 20 bytes
	p2 := writeFile(t, dir, "b.jsonl", "beta\n")                 // 5 bytes
	s := newTestServer(t)

	st := runPass(t, s, dir)
	if want := (Stats{Scanned: 2, Replaced: 2, Bytes: 25}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	if s.auth != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want bearer token", s.auth)
	}
	if s.machine != "testbox" {
		t.Fatalf("manifest machine = %q, want testbox", s.machine)
	}
	assertRaw(t, s, p1, "alpha one\nalpha two\n")
	assertRaw(t, s, p2, "beta\n")
}

// A file bigger than one chunk lands as a replace then appends of whole lines.
func TestBigFileUploadsInChunks(t *testing.T) {
	defer func(old int) { maxChunk = old }(maxChunk)
	maxChunk = 12
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "alpha one\nalpha two\nbeta\n") // 10 + 10 + 5 bytes
	s := newTestServer(t)

	st := runPass(t, s, dir)
	if want := (Stats{Scanned: 1, Replaced: 1, Bytes: 25}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	if s.putCount() != 3 {
		t.Fatalf("PUTs = %d, want 3", s.putCount())
	}
	if put := s.lastPut(); put.offset != 20 || put.body != "beta\n" {
		t.Fatalf("last PUT offset %d body %q, want offset 20 body %q", put.offset, put.body, "beta\n")
	}
	assertRaw(t, s, p, "alpha one\nalpha two\nbeta\n")
}

// A second pass with nothing new sends nothing.
func TestSecondPassSendsNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.jsonl", "alpha one\nalpha two\n")
	writeFile(t, dir, "b.jsonl", "beta\n")
	s := newTestServer(t)

	runPass(t, s, dir)
	before := s.putCount()
	st := runPass(t, s, dir)
	if want := (Stats{Scanned: 2, Unchanged: 2}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	if s.putCount() != before {
		t.Fatalf("second pass made %d PUTs, want 0", s.putCount()-before)
	}
}

// A file whose mtime is older than the manifest's UpdatedMs (minus skew) is
// skipped unread; touching it puts it back in play.
func TestOldMtimeSkipsRead(t *testing.T) {
	dir := t.TempDir()
	p1 := writeFile(t, dir, "a.jsonl", "alpha one\nalpha two\n") // 20 bytes
	writeFile(t, dir, "b.jsonl", "beta\n")
	s := newTestServer(t)
	runPass(t, s, dir)

	updated := time.Now().UnixMilli()
	s.updatedMs = updated
	// Same length, different first line: a read would re-send the whole file,
	// so Bytes 0 proves the file was never opened.
	writeFile(t, dir, "a.jsonl", "ALPHA one\nalpha two\n")
	old := time.UnixMilli(updated - time.Hour.Milliseconds())
	if err := os.Chtimes(p1, old, old); err != nil {
		t.Fatal(err)
	}

	before := s.putCount()
	st := runPass(t, s, dir)
	if want := (Stats{Scanned: 2, Unchanged: 2}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	if s.putCount() != before {
		t.Fatalf("skip pass made %d PUTs, want 0", s.putCount()-before)
	}
	assertRaw(t, s, p1, "alpha one\nalpha two\n")

	now := time.Now()
	if err := os.Chtimes(p1, now, now); err != nil {
		t.Fatal(err)
	}
	st = runPass(t, s, dir)
	if want := (Stats{Scanned: 2, Replaced: 1, Unchanged: 1, Bytes: 20}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	assertRaw(t, s, p1, "ALPHA one\nalpha two\n")
}

// Growing a file sends exactly the new line, at the server's old size.
func TestAppendSendsOnlyNewLine(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "alpha one\n")
	s := newTestServer(t)
	runPass(t, s, dir)

	writeFile(t, dir, "a.jsonl", "alpha one\ngamma three\n")
	st := runPass(t, s, dir)
	if want := (Stats{Scanned: 1, Appended: 1, Bytes: 12}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	put := s.lastPut()
	if put.offset != 10 || put.body != "gamma three\n" {
		t.Fatalf("PUT offset %d body %q, want offset 10 body %q", put.offset, put.body, "gamma three\n")
	}
	assertRaw(t, s, p, "alpha one\ngamma three\n")
}

// A trailing partial line waits; completing it sends the whole line.
func TestPartialTrailingLineWaits(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "one\ntw")
	s := newTestServer(t)

	st := runPass(t, s, dir)
	if want := (Stats{Scanned: 1, Replaced: 1, Bytes: 4}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	assertRaw(t, s, p, "one\n")

	st = runPass(t, s, dir)
	if want := (Stats{Scanned: 1, Unchanged: 1}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}

	writeFile(t, dir, "a.jsonl", "one\ntwo\n")
	st = runPass(t, s, dir)
	if want := (Stats{Scanned: 1, Appended: 1, Bytes: 4}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	put := s.lastPut()
	if put.offset != 4 || put.body != "two\n" {
		t.Fatalf("PUT offset %d body %q, want offset 4 body %q", put.offset, put.body, "two\n")
	}
	assertRaw(t, s, p, "one\ntwo\n")
}

// Rewriting the first line changes the head hash, so the whole file is
// re-sent at offset 0.
func TestRewrittenFirstLineResendsWhole(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "one\ntwo\n")
	s := newTestServer(t)
	runPass(t, s, dir)

	writeFile(t, dir, "a.jsonl", "ONE\ntwo\n")
	st := runPass(t, s, dir)
	if want := (Stats{Scanned: 1, Replaced: 1, Bytes: 8}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	put := s.lastPut()
	if put.offset != 0 || put.body != "ONE\ntwo\n" {
		t.Fatalf("PUT offset %d body %q, want offset 0 body %q", put.offset, put.body, "ONE\ntwo\n")
	}
	assertRaw(t, s, p, "ONE\ntwo\n")
}

// A stale manifest size draws one 409; when the extra server bytes are this
// client's own earlier append whose response was lost, the retry re-decides
// and appends from the server's real size.
func TestConflictRecoversInSamePass(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "one\ntwo\n")
	s := newTestServer(t)
	runPass(t, s, dir)
	base := s.putCount()

	// The last pass's append ("zz\n") committed on the server but the response
	// was lost, and the client kept writing. Manifest still says 8.
	s.appendDirect(t, p, "zz\n") // server now holds 11 bytes
	s.stale[wire.SrcID(p)] = 8
	writeFile(t, dir, "a.jsonl", "one\ntwo\nzz\nthree\n")

	st := runPass(t, s, dir)
	if want := (Stats{Scanned: 1, Appended: 1, Bytes: 6}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	s.mu.Lock()
	puts := append([]fakePut(nil), s.puts...)
	s.mu.Unlock()
	if len(puts) != base+2 {
		t.Fatalf("got %d PUTs (base %d), want 2 new (one conflicted, one retry)", len(puts), base)
	}
	if puts[base].offset != 8 || !puts[base].conflicted {
		t.Fatalf("first PUT offset %d conflicted=%v, want offset 8 and a 409", puts[base].offset, puts[base].conflicted)
	}
	if puts[base+1].offset != 11 || puts[base+1].body != "three\n" {
		t.Fatalf("retry PUT offset %d body %q, want offset 11 body %q", puts[base+1].offset, puts[base+1].body, "three\n")
	}
	assertRaw(t, s, p, "one\ntwo\nzz\nthree\n")
}

// A stale manifest size plus foreign server bytes leaves no safe seam to
// append at: the retry falls back to replacing the whole file.
func TestConflictWithForeignBytesResendsWhole(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "one\ntwo\n")
	s := newTestServer(t)
	runPass(t, s, dir)
	base := s.putCount()

	s.appendDirect(t, p, "zz\n") // another writer's bytes
	s.stale[wire.SrcID(p)] = 8
	writeFile(t, dir, "a.jsonl", "one\ntwo\nthree\n")

	st := runPass(t, s, dir)
	if want := (Stats{Scanned: 1, Replaced: 1, Bytes: 14}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	s.mu.Lock()
	puts := append([]fakePut(nil), s.puts...)
	s.mu.Unlock()
	if len(puts) != base+2 || !puts[base].conflicted || puts[base+1].offset != 0 || puts[base+1].body != "one\ntwo\nthree\n" {
		t.Fatalf("new PUTs = %+v (base %d), want a conflicted offset 8 then whole at offset 0", puts[base:], base)
	}
	assertRaw(t, s, p, "one\ntwo\nthree\n")
}

// The launchd plist must be a valid plist on the machine that runs launchd.
func TestLaunchdPlistLints(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil is macOS-only")
	}
	out, err := exec.Command(plutil, "-lint", filepath.Join("..", "..", "ops", "launchd", "com.mnemonic.push.plist")).CombinedOutput()
	if err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("plutil -lint said %q, want OK", out)
	}
}
