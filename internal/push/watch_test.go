package push

import (
	"context"
	"io"
	"os"
	"path/filepath"
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
