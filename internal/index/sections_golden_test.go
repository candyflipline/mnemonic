package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/wire"
	"github.com/klauspost/compress/zstd"
)

// The sections' expectations are main 59796bc's own: the spans goldens and
// the claude-code literal tree are internal/cache/index_spans_test.go's, the
// two-day annotation is index_annotations_test.go's, the two-author session
// is index_session_authors_test.go's, ported to rows the engine builds parts
// from.

// goldenRows reads one parser golden, the input main's cache derives its
// spans from, into rows grouped by session, in event order with turn_seq
// current, the way buildIndex prepares a session before addSession.
func goldenRows(t *testing.T, name string) (string, map[string][]*Row) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "goldens", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Harness string `json:"harness"`
		Records []struct {
			EventRows []json.RawMessage `json:"event_rows"`
		} `json:"records"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	bySession := map[string][]*Row{}
	for _, rec := range g.Records {
		for _, line := range rec.EventRows {
			line = []byte(strings.ReplaceAll(string(line), `"<event_version>"`, "1"))
			var ev schema.Event
			if err := json.Unmarshal(line, &ev); err != nil {
				t.Fatal(err)
			}
			r := rowFrom(&ev)
			r.Author = "test"
			bySession[r.Session] = append(bySession[r.Session], &r)
		}
	}
	for _, rows := range bySession {
		sort.Slice(rows, func(i, j int) bool { return orderLess(rows[i], rows[j]) })
		users := 0
		for _, r := range rows {
			if r.ActorKind == "user" && r.EventKind == "message" {
				users++
			}
			if r.TurnIndex == 0 {
				r.TurnIndex = uint64(max(1, users))
			}
		}
	}
	return g.Harness, bySession
}

// spanRow is one row of testdata/spans/<golden>.json, main's expectations.
type spanRow struct {
	SpanID           string `json:"span_id"`
	ParentSpanID     string `json:"parent_span_id"`
	Kind             string `json:"kind"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	Start            string `json:"start"`
	End              string `json:"end"`
	InputTokens      uint64 `json:"input_tokens"`
	OutputTokens     uint64 `json:"output_tokens"`
	CacheReadTokens  uint64 `json:"cache_read_tokens"`
	CacheWriteTokens uint64 `json:"cache_write_tokens"`
	FirstEvent       string `json:"first_event"`
	LastEvent        string `json:"last_event"`
}

func spanRowsOf(spans []schema.Span) []spanRow {
	out := make([]spanRow, 0, len(spans))
	for _, s := range spans {
		out = append(out, spanRow{SpanID: s.SpanID, ParentSpanID: s.ParentSpanID,
			Kind: string(s.Kind), Name: s.Name, Status: string(s.Status),
			Start: s.Start.Format("2006-01-02T15:04:05.000"), End: s.End.Format("2006-01-02T15:04:05.000"),
			InputTokens: s.InputTokens, OutputTokens: s.OutputTokens,
			CacheReadTokens: s.CacheReadTokens, CacheWriteTokens: s.CacheWriteTokens,
			FirstEvent: firstUID(s.FirstEventUID), LastEvent: firstUID(s.LastEventUID)})
	}
	return out
}

func firstUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// TestSpansGoldens derives each golden's spans the way the part build does
// and compares them with main's goldens, in the cache's order (start, kind,
// span_id).
func TestSpansGoldens(t *testing.T) {
	for _, golden := range []string{"codex", "cursor", "cursor_sqlite", "pi_coding_agent", "prime_agent"} {
		t.Run(golden, func(t *testing.T) {
			harness, bySession := goldenRows(t, golden)
			var got []spanRow
			for sid, rows := range bySession {
				spans := buildSpans(harness, rows)
				if n := len(spans); n == 0 {
					t.Fatalf("%s: no spans for session %s", golden, sid)
				}
				sort.Slice(spans, func(i, j int) bool {
					if !spans[i].Start.Equal(spans[j].Start) {
						return spans[i].Start.Before(spans[j].Start)
					}
					if spans[i].Kind != spans[j].Kind {
						return spans[i].Kind < spans[j].Kind
					}
					return spans[i].SpanID < spans[j].SpanID
				})
				got = append(got, spanRowsOf(spans)...)
			}
			raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "spans", golden+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var want []spanRow
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got, "", "  ")
				t.Fatalf("spans:\n%s", g)
			}
		})
	}
}

// TestClaudeCodeSpanTree is internal/cache/index_spans_test.go's literal
// tree for the claude_code golden's session.
func TestClaudeCodeSpanTree(t *testing.T) {
	harness, bySession := goldenRows(t, "claude_code")
	const session = "7c666c01-d38e-4658-8650-854ffb5b626e"
	spans := buildSpans(harness, bySession[session])
	if len(spans) == 0 {
		t.Fatalf("no spans for %s", session)
	}
	sort.Slice(spans, func(i, j int) bool {
		if !spans[i].Start.Equal(spans[j].Start) {
			return spans[i].Start.Before(spans[j].Start)
		}
		return spans[i].Kind < spans[j].Kind
	})
	var got []string
	for _, s := range spans {
		cost := "null"
		if s.Cost != nil {
			cost = strconv.FormatFloat(*s.Cost, 'g', -1, 64)
		}
		got = append(got, strings.Join([]string{s.SpanID, orDash(s.ParentSpanID), string(s.Kind), s.Name,
			string(s.Status), strconv.FormatUint(s.InputTokens, 10), strconv.FormatUint(s.OutputTokens, 10),
			strconv.FormatUint(s.CacheReadTokens, 10), strconv.FormatUint(s.CacheWriteTokens, 10), cost,
			firstUID(s.FirstEventUID), firstUID(s.LastEventUID)}, " "))
	}
	want := strings.Join([]string{
		"5c1265bbc73a071b - invoke_agent claude-code ok 9 5 2 19630 null 3e2f31f3 30afddf1",
		"055bfbac1f915da7 5c1265bbc73a071b turn turn 1 ok 9 5 2 19630 null 3e2f31f3 30afddf1",
		"93d47cb9e47a4461 055bfbac1f915da7 chat claude-opus-4-5-20251101 ok 9 5 2 19630 null 8814c5f2 d18fa55c",
		"712db1ba0f646f0e 93d47cb9e47a4461 execute_tool Read ok 0 0 0 0 null b7a60c19 8e640ed7",
	}, "\n")
	if strings.Join(got, "\n") != want {
		t.Fatalf("spans:\n%s\nwant:\n%s", strings.Join(got, "\n"), want)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// writeSource lays one raw source the way push does: zstd lines and a
// manifest, under author/machine/harness.
func writeSource(t *testing.T, root, author, harness, srcID, sourceName string, lines []byte) {
	t.Helper()
	zst := (&zstd.Encoder{}).EncodeAll(lines, nil)
	dir := filepath.Join(root, "raw", "author="+author, "machine=m1", "harness="+harness)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(map[string]any{"src_id": srcID, "harness": harness, "source_name": sourceName,
		"path": sourceName, "size": len(lines), "head_hash": "h", "updated_ms": 1, "zst_size": len(zst)})
	if err := os.WriteFile(filepath.Join(dir, srcID+".json"), m, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, srcID+".jsonl.zst"), zst, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAnnotationSentOnTwoDaysIsOneRow is internal/cache/
// index_annotations_test.go's case: the same annotation line in two sources
// is one annotation row carrying its author.
func TestAnnotationSentOnTwoDaysIsOneRow(t *testing.T) {
	line := `{"id":"a1","target":{"session_id":"s1"},"name":"quality","value":{"number":4},"source":"human","ts":"2026-10-21T10:00:00Z"}` + "\n"
	root, store := t.TempDir(), t.TempDir()
	for i, day := range []string{"2026-10-21", "2026-10-23"} {
		writeSource(t, root, "alice", schema.AnnotationHarness, wire.SrcID(day), "annotation/"+day, []byte(line))
		_ = i
	}
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	db, release := ix.Snapshot()
	defer release()
	ann := db.sessionAnnotations("s1")
	if len(ann) != 1 || ann[0].ID != "a1" || ann[0].Author != "alice" {
		b, _ := json.Marshal(ann)
		t.Fatalf("annotations: %s, want one a1 by alice", b)
	}
}

// TestSessionAuthorsEverySource is internal/cache/index_session_authors_
// test.go's case: a session two authors hold copies of keeps both authors,
// winner or not.
func TestSessionAuthorsEverySource(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "claude-code", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	root, store := t.TempDir(), t.TempDir()
	for _, author := range []string{"alice", "bob"} {
		writeSource(t, root, author, "claude-code", wire.SrcID(author), author+".jsonl", raw)
	}
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	db, release := ix.Snapshot()
	defer release()
	var got []string
	for sid := range db.sessPart {
		authors := db.sessionAuthors(sid)
		if len(authors) == 0 {
			continue
		}
		if !reflect.DeepEqual(authors, []string{"alice", "bob"}) {
			t.Fatalf("%s: authors %v, want [alice bob]", sid, authors)
		}
		spans := db.sessionSpans(sid)
		if len(spans) == 0 {
			t.Fatalf("%s: no spans", sid)
		}
		got = append(got, fmt.Sprintf("%d spans", len(spans)))
	}
	if len(got) == 0 {
		t.Fatal("no session carries authors")
	}
}
