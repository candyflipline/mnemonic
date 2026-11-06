package push

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
)

type recordingSink struct {
	mu    sync.Mutex
	paths []string
}

func (s *recordingSink) Sync(_ context.Context, paths []string) (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, paths...)
	return Stats{Appended: len(paths)}, nil
}

func (s *recordingSink) has(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.paths {
		if p == path {
			return true
		}
	}
	return false
}

func TestWatchPicksUpHarnessDirCreatedAfterStart(t *testing.T) {
	home := t.TempDir()
	present := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(present, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".codex", "sessions")
	cfg := config.Client{Sources: []config.SourceGlob{
		{Harness: "claude-code", SourceName: "claude", Glob: filepath.Join(present, "**", "*.jsonl")},
		{Harness: "codex", SourceName: "codex", Glob: filepath.Join(base, "**", "*.jsonl")},
	}}
	sink := &recordingSink{}
	w := newWatcher(cfg, nil)
	w.sink = sink
	w.quiet, w.within = 50*time.Millisecond, 400*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx, io.Discard)
	}()
	defer func() {
		cancel()
		<-done
	}()
	time.Sleep(200 * time.Millisecond)

	dir := filepath.Join(base, "2026", "11")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(session, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3*w.within + time.Second)
	for !sink.has(session) {
		if time.Now().After(deadline) {
			t.Fatalf("%s not synced within the wake window after its harness dir appeared", session)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) lines() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), "\n")
}

func TestWatchBacksOffWhileServerUnreachable(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Client{
		KeyFile: filepath.Join(blocker, "key"),
		Sources: []config.SourceGlob{{Harness: "claude-code", SourceName: "claude", Glob: filepath.Join(projects, "**", "*.jsonl")}},
	}
	out := &lockedBuf{}
	w := newWatcher(cfg, nil)
	w.within = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run(ctx, out)
	}()
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done
	if got := out.lines(); got != 1 {
		t.Fatalf("%d dial errors in 1.5s, want 1 (the first dial at start, then the %s backoff)", got, errorRetryMin)
	}
}
