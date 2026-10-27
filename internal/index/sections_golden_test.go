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

func buildSpans(harness string, rows []*Row) []schema.Span {
	f, ok := spanBuilders[harness]
	if !ok || len(rows) == 0 {
		return nil
	}
	return f(harness, rows[0].Session, func(yield func(*borrowedRow) bool) {
		for _, r := range rows {
			if !yield((*borrowedRow)(r)) {
				return
			}
		}
	})
}

func firstUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

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

func writeSource(t *testing.T, root, author, harness, srcID, sourceName string, frames ...[]byte) {
	t.Helper()
	var zst, lines []byte
	for _, f := range frames {
		zst = (&zstd.Encoder{}).EncodeAll(f, zst)
		lines = append(lines, f...)
	}
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

func TestOtlpSpans(t *testing.T) {
	row := func(uid, item, origin, kind string, in uint64, cost string) *Row {
		r := &Row{UID: uid, Session: "s", Harness: "otlp", Author: "alice", PayloadType: "span",
			TraceID: "t", ItemID: item, OriginEventID: origin, OpKind: kind, InputTokens: in,
			Bits: bitTimeOK, TimeUs: 1_000_000}
		if cost != "" {
			r.Native = `{"cost_usd":` + cost + `}`
		}
		return r
	}
	rows := []*Row{
		row("a", "A", "", "invoke_agent", 1, "9"),
		row("c1", "C1", "a", "chat", 10, "0.5"),
		row("c2", "C2", "a", "chat", 20, ""),
		row("x", "X", "a", "", 0, ""),
		row("t", "T", "x", "execute_tool", 0, ""),
		row("p", "P", "q", "chat", 0, ""),
		row("q", "Q", "p", "chat", 0, ""),
	}
	id := func(item string) string { return spanIDHex("otlp", "alice", "t", item) }
	got := map[string]schema.Span{}
	for _, s := range buildSpans("otlp", rows) {
		got[s.SpanID] = s
	}
	want := map[string]string{
		id("A"): "", id("C1"): id("A"), id("C2"): id("A"), id("T"): id("A"), id("P"): "", id("Q"): id("P"),
	}
	if len(got) != len(want) {
		t.Fatalf("%d spans, want %d", len(got), len(want))
	}
	for span, parent := range want {
		if got[span].ParentSpanID != parent {
			t.Errorf("span %s: parent %q, want %q", span, got[span].ParentSpanID, parent)
		}
	}
	a := got[id("A")]
	if a.InputTokens != 30 || a.Cost == nil || *a.Cost != 0.5 {
		t.Errorf("invoke_agent: %d input tokens, cost %v; want 30 and 0.5", a.InputTokens, a.Cost)
	}
	rows[1].Native = ""
	if a := buildSpans("otlp", rows)[0]; a.Cost == nil || *a.Cost != 9 {
		t.Errorf("invoke_agent without chat costs: cost %v, want its own 9", a.Cost)
	}
}

func TestHotPartKeepsSectionAuthors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "claude-code", "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const sid = "7c666c01-d38e-4658-8650-854ffb5b626e"
	more := []byte(`{"type":"user","sessionId":"` + sid + `","cwd":"/work/claude-demo","uuid":"user-3","timestamp":"2026-01-19T15:59:00.000Z","message":{"role":"user","content":"And the tests."}}` + "\n")
	note := []byte(`{"id":"n1","target":{"session_id":"` + sid + `"},"name":"quality","value":{"number":4},"source":"human","ts":"2026-10-21T10:00:00Z"}` + "\n")

	root, store, coldRoot, coldStore := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	put := func(root string, alice ...[]byte) {
		writeSource(t, root, "alice", "claude-code", wire.SrcID("a"), "alice.jsonl", alice...)
		writeSource(t, root, "bob", "claude-code", wire.SrcID("b"), "bob.jsonl", raw)
		writeSource(t, root, "carol", schema.AnnotationHarness, wire.SrcID("c"), "annotation/2026-10-21", note)
	}
	put(root, raw)
	if _, err := Ingest(root, store, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	put(root, raw, more)
	if _, err := Ingest(root, store, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	cur, err := readManifest(store)
	if err != nil {
		t.Fatalf("hot parts %v, err %v", cur.Hot, err)
	}
	deltas := 0
	for _, id := range cur.Hot {
		if _, ok, _ := readDelta(partDir(store, id)); ok {
			deltas++
		}
	}
	if deltas != 1 {
		t.Fatalf("%d hot parts carry a delta, want 1 (the session component's append)", deltas)
	}

	put(coldRoot, raw, more)
	if _, err := Ingest(coldRoot, coldStore, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	read := func(store string) ([]string, []schema.Annotation) {
		ix, err := Open(store)
		if err != nil {
			t.Fatal(err)
		}
		defer ix.Close()
		db, release := ix.Snapshot()
		defer release()
		return db.sessionAuthors(sid), db.sessionAnnotations(sid)
	}
	hotAuthors, hotAnn := read(store)
	coldAuthors, coldAnn := read(coldStore)
	if want := []string{"alice", "bob"}; !reflect.DeepEqual(coldAuthors, want) || !reflect.DeepEqual(hotAuthors, want) {
		t.Fatalf("authors: hot %v, cold %v, want %v", hotAuthors, coldAuthors, want)
	}
	if len(hotAnn) != 1 || hotAnn[0].ID != "n1" || hotAnn[0].Author != "carol" || len(coldAnn) != 1 || coldAnn[0].Author != "carol" {
		t.Fatalf("annotations: hot %+v, cold %+v, want n1 by carol", hotAnn, coldAnn)
	}
}
