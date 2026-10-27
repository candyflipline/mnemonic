//go:build server

package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/index"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/redact"
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
