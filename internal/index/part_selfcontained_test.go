package index

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestOpenedPartListsSessionsAfterItsDirIsGone(t *testing.T) {
	session, err := os.ReadFile("../../testdata/fixtures/claude-code/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat(session, 10)
	enc, _ := zstd.NewWriter(nil)
	zst := enc.EncodeAll(raw, nil)
	root := t.TempDir()
	dir := filepath.Join(root, "raw", "author=a", "machine=m", "harness=claude-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(map[string]any{"src_id": "s1", "harness": "claude-code", "source_name": "claude",
		"path": "/p/s1.jsonl", "size": len(raw), "updated_ms": 1, "zst_size": len(zst)})
	if err := os.WriteFile(filepath.Join(dir, "s1.json"), m, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl.zst"), zst, 0o644); err != nil {
		t.Fatal(err)
	}
	store := t.TempDir()
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	cur, err := readManifest(store)
	if err != nil || len(cur.Parts) != 1 {
		t.Fatalf("parts %v, err %v", cur.Parts, err)
	}
	pd := partDir(store, cur.Parts[0])
	p, err := OpenPartDir(pd)
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenParts([]*Part{p})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := os.RemoveAll(pd); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"start_datetime":"2000-01-01T00:00:00Z","end_datetime":"2100-01-01T00:00:00Z"}`)
	out, _, err := Call(context.Background(), db, "list_sessions", args)
	if err != nil {
		t.Fatalf("list_sessions after the part dir was removed: %v", err)
	}
	if !bytes.Contains(out, []byte(`"session_id"`)) {
		t.Fatalf("no sessions listed: %s", out)
	}
}

func TestConcurrentToolCallsOnOneSnapshot(t *testing.T) {
	db, dir := newQueryDB(t)
	ctx := context.Background()
	claude := "7c666c01-d38e-4658-8650-854ffb5b626e"

	listArgs := `{"start_datetime":"2000-01-01T00:00:00Z","end_datetime":"2100-01-01T00:00:00Z"}`
	searchArgs := `{"query":"project"}`
	sdata, _, err := Call(ctx, db, "search_sessions", json.RawMessage(searchArgs))
	if err != nil {
		t.Fatal(err)
	}
	var hits struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(sdata, &hits); err != nil {
		t.Fatal(err)
	}
	if len(hits.Results) == 0 {
		t.Fatalf("search_sessions found nothing: %s", sdata)
	}
	openArgs := `{"id":"` + hits.Results[0].ID + `"}`
	calls := []struct{ tool, args string }{
		{"open", openArgs},
		{"list_sessions", listArgs},
		{"search_sessions", searchArgs},
	}
	want := make([]string, len(calls))
	for i, c := range calls {
		data, _, err := Call(ctx, db, c.tool, json.RawMessage(c.args))
		if err != nil {
			t.Fatalf("%s: %v", c.tool, err)
		}
		want[i] = string(data)
	}
	var listed struct {
		Sessions []any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(want[1]), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 2 {
		t.Fatalf("list_sessions returned %d sessions, want 2", len(listed.Sessions))
	}

	racing, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer racing.Close()

	const lanes = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, lanes)
	for g := 0; g < lanes; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for round := 0; round < 25; round++ {
				for i, c := range calls {
					data, _, err := Call(ctx, racing, c.tool, json.RawMessage(c.args))
					if err != nil {
						errs[g] = fmt.Errorf("%s: %w", c.tool, err)
						return
					}
					if string(data) != want[i] {
						errs[g] = fmt.Errorf("%s: answer changed under concurrency", c.tool)
						return
					}
				}
				if a := racing.sessionAuthors(claude); len(a) != 1 || a[0] != "a" {
					errs[g] = fmt.Errorf("session authors of %s: %v", claude, a)
					return
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()
	for g, err := range errs {
		if err != nil {
			t.Fatalf("lane %d: %v", g, err)
		}
	}
}
