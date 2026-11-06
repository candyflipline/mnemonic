//go:build server

package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/index"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/redact"
	"github.com/candyflipline/mnemonic/internal/store"
)

func TestRefuseClientDir(t *testing.T) {
	home := t.TempDir()
	client := filepath.Join(home, ".mnemonic")
	if err := os.MkdirAll(filepath.Join(client, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(client, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(client, "sub"), filepath.Join(home, "sublink")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(home)
	darwin := runtime.GOOS == "darwin"

	cases := []struct {
		name, dir string
		refused   bool
	}{
		{"exact", client, true},
		{"trailing slash and dots", client + "/./", true},
		{"relative from home", ".mnemonic", true},
		{"case variant", filepath.Join(home, ".Mnemonic"), darwin},
		{"symlink to it", filepath.Join(home, "link"), true},
		{"symlink to a subdirectory", filepath.Join(home, "sublink"), true},
		{"existing subdirectory", filepath.Join(client, "sub"), true},
		{"subdirectory not yet created", filepath.Join(client, "a", "b"), true},
		{"through the symlink, not yet created", filepath.Join(home, "link", "a"), true},
		{"dotdot back into it", filepath.Join(home, "x", "..", ".mnemonic"), true},
		{"the default", filepath.Join(home, ".mnemonic-local"), false},
		{"a sibling sharing the prefix", filepath.Join(home, ".mnemonicx"), false},
		{"a fresh directory", filepath.Join(home, "elsewhere", "data"), false},
	}
	for _, c := range cases {
		err := refuseClientDir(c.dir, home)
		if (err != nil) != c.refused {
			t.Errorf("%s: refuseClientDir(%q) = %v, refused want %v", c.name, c.dir, err, c.refused)
		}
	}
}

func TestWakeCycleDrainsSettles(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "claude-code", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const sid = "7c666c01-d38e-4658-8650-854ffb5b626e"
	const other = "7c666c01-d38e-4658-8650-854ffb5b626f"
	dir := t.TempDir()
	files := make(map[string]string, 3)
	for name, sess := range map[string]string{"a": sid, "b": other, "c": other[:len(other)-1] + "0"} {
		p := filepath.Join(dir, name+".jsonl")
		if err := os.WriteFile(p, bytes.ReplaceAll(fixture, []byte(sid), []byte(sess)), 0o644); err != nil {
			t.Fatal(err)
		}
		files[name] = p
	}
	backdate := func(p string, d time.Duration) {
		old := time.Now().Add(-d)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := index.Open(layout.IndexDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	red, err := redact.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	srcs := []config.SourceGlob{{Harness: "claude-code", SourceName: "claude", Glob: filepath.Join(dir, "*.jsonl")}}
	sink := &indexSink{ix: ix, srcs: srcs, author: "tester", machine: "testbox", red: red}
	if err := firstIngest(ix, srcs, logger, "tester", "testbox", red); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	backdate(files["c"], 20*time.Minute)
	if _, err := sink.Sync(context.Background(), nil); err != nil {
		t.Fatalf("settle sync: %v", err)
	}
	if ix.LastCycle().ColdParts == 0 {
		t.Fatal("the settle sync left no cold shard, so the append cycle would settle b itself and the test would pin nothing")
	}
	backdate(files["b"], 25*time.Minute)
	line := bytes.ReplaceAll(
		bytes.SplitN(fixture, []byte("\n"), 2)[0],
		[]byte(`"uuid":"user-1"`), []byte(`"uuid":"user-9"`))
	f, err := os.OpenFile(files["a"], os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	wake := make(chan struct{}, 1)
	sink.wakeCycle(context.Background(), wake, logger)
	if got := ix.SweepStale(); got != 0 {
		t.Fatalf("after the wake cycle SweepStale() = %d, want 0: the cycle that parsed a's append settles nothing, and the wake path left b hot", got)
	}
}

func TestLocalRefusesForeignHostAndOrigin(t *testing.T) {
	const sid = "7c666c01-d38e-4658-8650-854ffb5b626e"
	dir := t.TempDir()
	ix, err := index.Open(layout.IndexDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	red, err := redact.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	glob := filepath.Join("..", "..", "testdata", "fixtures", "claude-code", "*.jsonl")
	srcs := []config.SourceGlob{{Harness: "claude-code", SourceName: "claude", Glob: glob}}
	if err := firstIngest(ix, srcs, log.New(io.Discard, "", 0), "tester", "testbox", red); err != nil {
		t.Fatal(err)
	}
	ctl, err := index.LoadControls(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ts := httptest.NewUnstartedServer(nil)
	ts.Config = localServer("127.0.0.1:0", "tester", ix, ctl, st)
	ts.Start()
	defer ts.Close()
	port := ts.Listener.Addr().(*net.TCPAddr).Port

	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	cases := []struct {
		route, host, origin string
		want                int
	}{
		{"mcp", "evil.example", "", http.StatusForbidden},
		{"export", "evil.example", "", http.StatusForbidden},
		{"mcp", "evil.example:" + strconv.Itoa(port), "", http.StatusForbidden},
		{"export", "evil.example:7781", "", http.StatusForbidden},
		{"mcp", "127.0.0.1:" + strconv.Itoa(port), "", http.StatusOK},
		{"export", "127.0.0.1:" + strconv.Itoa(port), "", http.StatusOK},
		{"mcp", "localhost:7781", "", http.StatusOK},
		{"export", "localhost:7781", "", http.StatusOK},
		{"mcp", "[::1]:7781", "", http.StatusOK},
		{"export", "[::1]:7781", "", http.StatusOK},
		{"export", "localhost", "", http.StatusOK},
		{"mcp", "127.0.0.1:7781", "http://evil.example", http.StatusForbidden},
		{"export", "127.0.0.1:7781", "http://evil.example", http.StatusForbidden},
		{"export", "127.0.0.1:7781", "null", http.StatusForbidden},
		{"export", "127.0.0.1:7781", "http://localhost:7781", http.StatusOK},
		{"mcp", "127.0.0.1:7781", "http://127.0.0.1:7781", http.StatusOK},
	}
	for _, c := range cases {
		var req *http.Request
		if c.route == "mcp" {
			req, err = http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(initialize))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
		} else {
			req, err = http.NewRequest(http.MethodGet, ts.URL+"/v1/export/otlp?session="+sid, nil)
			if err != nil {
				t.Fatal(err)
			}
		}
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s Host %q Origin %q: status %d, want %d: %.60q", c.route, c.host, c.origin, resp.StatusCode, c.want, body)
		}
	}
}
