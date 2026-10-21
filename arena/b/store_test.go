package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

// One integration table covers real parser state, published queries, append,
// idempotence, failed publication, and recovery without the private sample.
func TestStore(t *testing.T) {
	t.Run("path_index_recall", func(t *testing.T) {
		db := &Store{}
		for i, input := range []string{
			`{"path":"/work/src/a.go"}`,
			`{"files":{"file_path":"/work/src/a.go"}}`,
			`{"file_path":["/work/src/a.go","/work/other.go"]}`,
			`{"file_path":["/work/x]src/a.go"]}`,
			`{"command":"cat /work/src/a.go; cat src/a.go; echo x=/work/src/a.go"}`,
			`{"cmd":"cat /work/foo(bar/a.go; cat /work/space name.go"}`,
			`{"path":"/work/src/\u0061.go"}`,
			`{"path":"/work/src/a.go",broken}`,
		} {
			db.events = append(db.events, &event{Columns: Columns{EventUID: fmt.Sprint(i), SessionID: "s", ToolPhase: "request", ToolName: "Read"}, IO: &toolIO{Input: input}, Time: 1, Turn: 1})
		}
		db.buildPaths()
		for _, rel := range []string{"a.go", "src/a.go", "x]src/a.go", "other.go", "foo(bar/a.go", "space name.go", "/work/src/a.go", "missing.go"} {
			q := faQuery{Rel: rel, MaxRows: 100}
			indexed, err := faTouches(context.Background(), db, q)
			if err != nil {
				t.Fatal(err)
			}
			paths := db.paths
			db.paths = nil
			scan, err := faTouches(context.Background(), db, q)
			db.paths = paths
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(indexed, scan) {
				t.Fatalf("index missed a path for %q", rel)
			}
			if rel == "src/a.go" && len(indexed) != 5 {
				t.Fatalf("literal touch count = %d, want 5", len(indexed))
			}
		}
	})

	for _, tc := range []struct {
		name, harness, file, path, sid string
		events                         int
	}{
		{"codex", "codex", "codex/session.jsonl", "/fixtures/codex/session.jsonl", "019c5f6a-49bd-7920-ac67-1dd8e33b0e95", 15},
		{"claude", "claude-code", "claude-code/session.jsonl", "/fixtures/claude-code/session.jsonl", "7c666c01-d38e-4658-8650-854ffb5b626e", 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../../testdata/fixtures", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			dir := t.TempDir()
			clean := t.TempDir()
			base := filepath.Join(root, "raw", "author=test", "machine=m1", "harness="+tc.harness)
			if err = os.MkdirAll(base, 0700); err != nil {
				t.Fatal(err)
			}
			src := wire.Source{SrcID: "fixture", Harness: tc.harness, SourceName: "golden-" + tc.harness, Path: tc.path, UpdatedMs: 1767225600000}
			firstLine := bytes.IndexByte(raw, '\n') + 1
			src.HeadHash = wire.HeadHash(raw[:firstLine])
			cut := len(raw) / 2
			cut += bytes.IndexByte(raw[cut:], '\n') + 1
			z, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
			if err != nil {
				t.Fatal(err)
			}
			defer z.Close()
			head, tail := z.EncodeAll(raw[:cut], nil), z.EncodeAll(raw[cut:], nil)
			save := func(b []byte, size int) {
				t.Helper()
				src.Size = int64(size)
				m, _ := json.Marshal(struct {
					wire.Source
					ZstSize int64 `json:"zst_size"`
				}{src, int64(len(b))})
				if err := os.WriteFile(filepath.Join(base, "fixture.json"), m, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(base, "fixture.jsonl.zst"), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			save(head, cut)
			if err = ingest(root, dir); err != nil {
				t.Fatal(err)
			}
			before, _ := readManifest(dir)
			cp := before.Sources["author=test/machine=m1/harness="+tc.harness+"/fixture.json"]
			if cp.State.Offset != uint64(cut) || cp.Compressed != int64(len(head)) || len(cp.Segments) != 1 {
				t.Fatal("first checkpoint mismatch")
			}
			// Bytes beyond the manifest's committed zstd boundary are ignored.
			rawPath := filepath.Join(base, "fixture.jsonl.zst")
			f, err := os.OpenFile(rawPath, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write(tail[:len(tail)/2]); err != nil {
				t.Fatal(err)
			}
			f.Close()
			beforeBytes, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err = ingest(root, dir); err != nil {
				t.Fatal(err)
			}
			afterBytes, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if !bytes.Equal(beforeBytes, afterBytes) {
				t.Fatal("uncommitted tail changed checkpoint")
			}
			save(append(append([]byte{}, head...), tail...), len(raw))
			if err = ingest(root, dir); err != nil {
				t.Fatal(err)
			}
			m, _ := readManifest(dir)
			cp = m.Sources["author=test/machine=m1/harness="+tc.harness+"/fixture.json"]
			if cp.State.Offset != uint64(len(raw)) || len(cp.Segments) != 2 {
				t.Fatal("append did not retain the first segment")
			}
			b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err = ingest(root, dir); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if !bytes.Equal(b, after) {
				t.Fatal("no-op ingest changed manifest")
			}
			if err = ingest(root, clean); err != nil {
				t.Fatal(err)
			}
			db, err := loadStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			full, err := loadStore(clean)
			if err != nil {
				t.Fatal(err)
			}
			defer full.Close()
			// Literal values come from the checked-in public parser golden, not private data.
			if len(db.events) != tc.events || len(db.sessions) != 1 {
				t.Fatalf("events=%d sessions=%d", len(db.events), len(db.sessions))
			}
			for _, query := range []struct{ tool, args string }{
				{"open", `{"id":"` + formatSessionID(tc.sid) + `","limit":2}`},
				{"search_sessions", `{"query":"inspect test","event_types":["user_input","assistant_response","tool_call","tool_response"]}`},
				{"file_attention", `{"path":"Cargo.toml","scope":"all","granularity":"events"}`},
				{"list_sessions", `{"start_datetime":"2020-01-01T00:00:00Z","end_datetime":"2030-01-01T00:00:00Z"}`},
			} {
				tool, _ := Lookup(query.tool)
				data, text, err := tool.Call(context.Background(), db, json.RawMessage(query.args))
				if err != nil {
					t.Fatalf("%s: %v", query.tool, err)
				}
				expected, wtext, err := tool.Call(context.Background(), full, json.RawMessage(query.args))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(data, expected) || text != wtext {
					t.Fatalf("%s incremental != clean", query.tool)
				}
				if query.tool == "open" {
					s := data.(map[string]any)["session"].(map[string]any)
					if s["id"] != formatSessionID(tc.sid) || s["event_count"] == uint64(0) {
						t.Fatal("missing expected session")
					}
				}
				if query.tool == "search_sessions" && data.(map[string]any)["result_count"] == 0 {
					t.Fatal("expected cargo test hits")
				}
			}
			// Old readers pin heavy file descriptors across a source replacement.
			save(head, cut)
			if err = ingest(root, dir); err != nil {
				t.Fatal(err)
			}
			clear(db.heavyCache)
			db.heavyOrder = nil
			db.heavyBytes = 0
			opened, _, err := openTool{}.Call(context.Background(), db, json.RawMessage(`{"id":"`+formatSessionID(tc.sid)+`","limit":2}`))
			if err != nil {
				t.Fatal(err)
			}
			if opened.(map[string]any)["session"].(map[string]any)["event_count"] != uint64(tc.events) {
				t.Fatal("pinned snapshot changed")
			}
			if stampManifest(dir) == db.stamp {
				t.Fatal("source replacement did not change snapshot stamp")
			}
			save(append(append([]byte{}, head...), tail...), len(raw))
			if err = ingest(root, dir); err != nil {
				t.Fatal(err)
			}
			b, _ = os.ReadFile(filepath.Join(dir, "manifest.json"))
			// Simulate a killed writer's unreferenced file, then verify reclamation.
			if err = os.WriteFile(filepath.Join(dir, ".segment-orphan"), []byte("incomplete"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = ingest(root, dir); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(dir, ".segment-orphan")); !os.IsNotExist(err) {
				t.Fatal("orphan not reclaimed")
			}
			// An incomplete zstd append must not advance the published checkpoint.
			save(append(append([]byte{}, head...), tail[:len(tail)/2]...), len(raw))
			if err = ingest(root, dir); err == nil {
				t.Fatal("accepted truncated frame")
			}
			after, _ = os.ReadFile(filepath.Join(dir, "manifest.json"))
			if !bytes.Equal(b, after) {
				t.Fatal("failed ingest published manifest")
			}
			for _, bad := range []struct{ tool, args, want string }{
				{"search_sessions", `{"query":"a"}`, "query has no searchable terms (tokens shorter than 2 characters are excluded)"},
				{"open", `{"id":"event:bWlzc2luZw"}`, "event not found"},
				{"file_attention", `{"path":" Cargo.toml"}`, "path must not have leading or trailing whitespace"},
			} {
				tool, _ := Lookup(bad.tool)
				_, _, err := tool.Call(context.Background(), db, json.RawMessage(bad.args))
				if err == nil || strings.TrimSpace(err.Error()) != bad.want {
					t.Fatalf("error for %s: %v", bad.tool, err)
				}
			}
		})
	}
}
