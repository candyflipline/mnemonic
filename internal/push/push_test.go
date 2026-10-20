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

	"github.com/klauspost/compress/zstd"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
)

type identityRedactor struct{}

func (identityRedactor) Line(line []byte) []byte { return line }

type countingRedactor struct{ n int64 }

func (c *countingRedactor) Line(line []byte) []byte {
	c.n += int64(len(line))
	return line
}

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
	rawEnd  int64
	head    string
	harness string
	srcName string
	path    string
}

type fakePut struct {
	srcID      string
	offset     int64
	body, head string
	rawEnd     int64
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
			Size: size, RawEnd: src.rawEnd, HeadHash: src.head, UpdatedMs: s.updatedMs,
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
	rawEnd, err := strconv.ParseInt(r.URL.Query().Get("raw_end"), 10, 64)
	if err != nil {
		rawEnd = 0
	}
	if rawEnd < 0 {
		http.Error(w, "bad raw_end", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[srcID]
	if src == nil {
		src = &fakeSource{}
		s.sources[srcID] = src
	}
	s.puts = append(s.puts, fakePut{
		srcID: srcID, offset: offset, body: string(body), head: head, rawEnd: rawEnd,
		conflicted: offset != 0 && offset != int64(len(src.raw)),
	})
	switch {
	case offset == 0:
		src.raw = body
		src.rawEnd = rawEnd
		src.head = head
		src.harness = r.PathValue("harness")
		src.srcName = r.URL.Query().Get("source_name")
		src.path = r.URL.Query().Get("path")
	case offset == int64(len(src.raw)):
		src.raw = append(src.raw, body...)
		src.rawEnd = rawEnd
		src.head = head
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(wire.Conflict{Size: int64(len(src.raw)), HeadHash: src.head, RawEnd: src.rawEnd})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(wire.Source{
		SrcID: srcID, Harness: src.harness, SourceName: src.srcName, Path: src.path,
		Size: int64(len(src.raw)), RawEnd: src.rawEnd, HeadHash: src.head, UpdatedMs: 2,
	})
}

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

func TestFirstPassUploadsWholeFiles(t *testing.T) {
	dir := t.TempDir()
	p1 := writeFile(t, dir, "a.jsonl", "alpha one\nalpha two\n")
	p2 := writeFile(t, dir, "b.jsonl", "beta\n")
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

func TestBigFileUploadsInChunks(t *testing.T) {
	defer func(old int) { maxChunk = old }(maxChunk)
	maxChunk = 12
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "alpha one\nalpha two\nbeta\n")
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

type realStore struct {
	srv     *httptest.Server
	dataDir string
}

func newRealStore(t *testing.T) *realStore {
	t.Helper()
	data := t.TempDir()
	st := &store.Store{DataDir: data}
	srv := httptest.NewServer(auth.Middleware(map[string]string{auth.Hash("test-token"): "chris"})(st.Handler()))
	t.Cleanup(srv.Close)
	return &realStore{srv: srv, dataDir: data}
}

func realManifestSource(t *testing.T, rs *realStore, path string) wire.Source {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rs.srv.URL+manifestPath+"?machine=testbox", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := rs.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("manifest: status %d, want 200", res.StatusCode)
	}
	var man wire.Manifest
	if err := json.NewDecoder(res.Body).Decode(&man); err != nil {
		t.Fatal(err)
	}
	for _, src := range man.Sources {
		if src.SrcID == wire.SrcID(path) {
			return src
		}
	}
	t.Fatalf("manifest has no entry for %s", path)
	return wire.Source{}
}

func storedRaw(t *testing.T, dataDir, path string) string {
	t.Helper()
	k := layout.Key{Author: "chris", Machine: "testbox", Harness: "claude-code", SrcID: wire.SrcID(path)}
	f, err := os.Open(k.Raw(dataDir))
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

func resetRawEnd(t *testing.T, dataDir, path string) {
	t.Helper()
	k := layout.Key{Author: "chris", Machine: "testbox", Harness: "claude-code", SrcID: wire.SrcID(path)}
	b, err := os.ReadFile(k.Manifest(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		wire.Source
		ZstSize int64 `json:"zst_size"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m.RawEnd = 0
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(k.Manifest(dataDir), append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedFileBackfillsRawEndThenSkips(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "head line\n"+strings.Repeat("filler line\n", 99))
	rs := newRealStore(t)
	if _, err := Run(context.Background(), testConfig(rs.srv.URL, dir), identityRedactor{}, rs.srv.Client()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	resetRawEnd(t, rs.dataDir, p)

	st, err := Run(context.Background(), testConfig(rs.srv.URL, dir), identityRedactor{}, rs.srv.Client())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := (Stats{Scanned: 1, Unchanged: 1}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	src := realManifestSource(t, rs, p)
	if src.Size != 1198 || src.RawEnd != 1198 {
		t.Fatalf("manifest Size = %d RawEnd = %d, want 1198 and 1198", src.Size, src.RawEnd)
	}
	if got := storedRaw(t, rs.dataDir, p); got != "head line\n"+strings.Repeat("filler line\n", 99) {
		t.Fatalf("store holds %q, want the original bytes", got)
	}

	red := &countingRedactor{}
	st, err = Run(context.Background(), testConfig(rs.srv.URL, dir), red, rs.srv.Client())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := (Stats{Scanned: 1, Unchanged: 1}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	if red.n != 0 {
		t.Fatalf("Line saw %d bytes on the skip pass, want 0", red.n)
	}
}

func TestAppendTailRedactsOnlyNewBytes(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "head line\n"+strings.Repeat("filler line\n", 999))
	rs := newRealStore(t)
	if _, err := Run(context.Background(), testConfig(rs.srv.URL, dir), identityRedactor{}, rs.srv.Client()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body := "head line\n" + strings.Repeat("filler line\n", 999) + "new one\nnew two\n"
	writeFile(t, dir, "a.jsonl", body)
	red := &countingRedactor{}
	st, err := Run(context.Background(), testConfig(rs.srv.URL, dir), red, rs.srv.Client())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := (Stats{Scanned: 1, Appended: 1, Bytes: 16}); st != want {
		t.Fatalf("stats = %+v, want %+v", st, want)
	}
	if red.n != 9+7+7 {
		t.Fatalf("Line saw %d bytes, want %d", red.n, 9+7+7)
	}
	if got := storedRaw(t, rs.dataDir, p); got != body {
		t.Fatalf("store holds %q, want %q", got, body)
	}
	src := realManifestSource(t, rs, p)
	if src.Size != 12014 || src.RawEnd != 12014 {
		t.Fatalf("manifest Size = %d RawEnd = %d, want 12014 and 12014", src.Size, src.RawEnd)
	}
}

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

func TestConflictRecoversInSamePass(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "one\ntwo\n")
	s := newTestServer(t)
	runPass(t, s, dir)
	base := s.putCount()

	s.appendDirect(t, p, "zz\n")
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

func TestConflictWithForeignBytesResendsWhole(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.jsonl", "one\ntwo\n")
	s := newTestServer(t)
	runPass(t, s, dir)
	base := s.putCount()

	s.appendDirect(t, p, "zz\n")
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
