package index

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/ql"
	"github.com/candyflipline/mnemonic/internal/schema"
	"github.com/candyflipline/mnemonic/internal/store"
)

func newQueryDB(t *testing.T) (*DB, string) {
	t.Helper()
	root := t.TempDir()
	for _, s := range []struct{ author, harness, file string }{
		{"a", "claude-code", "../../testdata/fixtures/claude-code/session.jsonl"},
		{"b", "codex", "../../testdata/fixtures/codex/session.jsonl"},
	} {
		session, err := os.ReadFile(s.file)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "raw", "author="+s.author, "machine=m", "harness="+s.harness)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		enc, _ := zstd.NewWriter(nil)
		zst := enc.EncodeAll(session, nil)
		m, _ := json.Marshal(map[string]any{"src_id": "s1", "harness": s.harness, "source_name": s.harness,
			"path": "/p/s1.jsonl", "size": len(session), "updated_ms": 1, "zst_size": len(zst)})
		if err := os.WriteFile(filepath.Join(dir, "s1.json"), m, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "s1.jsonl.zst"), zst, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if _, err := Ingest(root, dir, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dir
}

func queryCall(t *testing.T, ctx context.Context, db *DB, q, from, to string, limit *int) (map[string]any, string, error) {
	t.Helper()
	args := map[string]any{"query": q, "from": from, "to": to}
	if limit != nil {
		args["limit"] = *limit
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	data, text, err := Call(ctx, db, "query", raw)
	if err != nil {
		return nil, text, err
	}
	var d map[string]any
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	return d, text, nil
}

func queryRows(t *testing.T, d map[string]any) string {
	t.Helper()
	b, err := json.Marshal(d["rows"])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestQueryResultsOnTheEngine(t *testing.T) {
	db, _ := newQueryDB(t)
	ctx := context.Background()
	jan := [2]string{"2026-01-01T00:00:00Z", "2026-01-31T00:00:00Z"}
	feb := [2]string{"2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"}
	all := [2]string{"2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z"}
	for _, c := range []struct {
		name      string
		q         string
		rng       [2]string
		limit     *int
		wantRows  string
		wantText  string
		wantCount any
	}{
		{"grouped count by harness", "from: events | dimensions: harness | measures: count() as n | sort: harness", all, nil,
			`[["claude-code",6],["codex",15]]`, "", nil},
		{"events of january", "from: events | measures: count()", jan, nil, `[[6]]`, "", nil},
		{"events of february", "from: events | measures: count()", feb, nil, `[[15]]`, "", nil},
		{"a session overlapping the range is a sessions row", "from: sessions | measures: count()", jan, nil, `[[1]]`, "", nil},
		{"no session overlaps april", "from: sessions | measures: count()", [2]string{"2026-04-01T00:00:00Z", "2026-04-02T00:00:00Z"}, nil,
			`[[0]]`, "query returned 1 row(s) for 2026-04-01T00:00:00Z to 2026-04-02T00:00:00Z.\ncount()\n0\n", nil},
		{"a filter over author or harness", "from: events | filter: author = 'a' or harness = 'codex' | measures: count()", all, nil, `[[21]]`, "", nil},
		{"order survives the row cap", "from: events | select: event_ts | sort: event_ts | limit: 2", jan, nil,
			`[["2026-01-19T15:58:40Z"],["2026-01-19T15:58:41.421Z"]]`, "", nil},
		{"turns of the sessions in range", "from: turns | measures: count()", jan, nil, `[[1]]`, "", nil},
		{"a heavy column read from the record", "from: events | filter: latency_ms > 0 | select: latency_ms", jan, nil, `[[12]]`, "", nil},
		{"the parse stamps model, codex inherits the limit name", "from: events | filter: model != '' | dimensions: model | measures: count() | sort: model", all, nil,
			`[["claude-opus-4-5-20251101",3],["gpt-5.3-codex",8],["gpt-5.3-codex-spark",6]]`, "", nil},
		{"absent heavy values are the zero fill, not null", "from: events | filter: input_tokens = 0 | measures: count()", jan, nil, `[[3]]`, "", nil},
		{"a present heavy number reads the parse's count", "from: events | filter: input_tokens > 0 | dimensions: model | measures: count()", jan, nil,
			`[["claude-opus-4-5-20251101",3]]`, "", nil},
		{"a list column the parse filled", "from: events | filter: content_types includes 'output_text' | measures: count()", feb, nil, `[[1]]`, "", nil},
		{"the claude-code text block stamps its type", "from: events | filter: content_types includes 'text' | measures: count()", jan, nil, `[[1]]`, "", nil},
		{"names are case-insensitive", "FROM: EVENTS | MEASURES: COUNT(*)", all, nil, `[[21]]`, "", nil},
	} {
		d, _, err := queryCall(t, ctx, db, c.q, c.rng[0], c.rng[1], c.limit)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := queryRows(t, d); got != c.wantRows {
			t.Fatalf("%s: rows %s, want %s", c.name, got, c.wantRows)
		}
		if c.wantText != "" {
			if _, text, err := Call(ctx, db, "query", mustRaw(map[string]any{"query": c.q, "from": c.rng[0], "to": c.rng[1]})); err != nil || text != c.wantText {
				t.Fatalf("%s: text %q (%v), want %q", c.name, text, err, c.wantText)
			}
		}
	}
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestQueryRowCapOnTheEngine(t *testing.T) {
	db, _ := newQueryDB(t)
	ctx := context.Background()
	two := 2
	d, _, err := queryCall(t, ctx, db, "from: events | select: event_uid", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", &two)
	if err != nil {
		t.Fatal(err)
	}
	if d["row_count"] != float64(2) || d["truncated"] != true || d["limit"] != float64(2) {
		t.Fatalf("limit 2: row_count %v truncated %v limit %v", d["row_count"], d["truncated"], d["limit"])
	}
	d, _, err = queryCall(t, ctx, db, "from: events | select: event_uid", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if d["row_count"] != float64(21) || d["truncated"] != false || d["limit"] != float64(100) {
		t.Fatalf("default limit: row_count %v truncated %v limit %v", d["row_count"], d["truncated"], d["limit"])
	}
	for _, limit := range []int{0, 10001} {
		_, _, err := queryCall(t, ctx, db, "from: events | measures: count()", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", &limit)
		if err == nil || err.Error() != "limit must be between 1 and 10000" {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
}

func TestQueryRefusalsOnTheEngine(t *testing.T) {
	db, _ := newQueryDB(t)
	ctx := context.Background()
	const views = "; query one of annotations, events, sessions, spans, turns"
	for _, c := range []struct {
		q, from, to    string
		limit          *int
		want           string
		wantArgError   bool
		wantParseError bool
	}{
		{"SELECT count(*) FROM events", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", nil,
			"1:1: SQL is not accepted; write clauses, for example: from: events | filter: harness = 'codex' | select: session_id, event_ts", true, true},
		{"from: lake_files", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", nil,
			`1:7: relation "lake_files" is not queryable` + views, true, true},
		{"from: events | select: nope", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", nil,
			`1:24: events has no column "nope"`, true, true},
		{"", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", nil, "query is required", true, false},
		{"from: events", "", "2026-03-01T00:00:00Z", nil, "from is required (an RFC 3339 timestamp)", true, false},
		{"from: events", "2026-01-01T00:00:00Z", "", nil, "to is required (an RFC 3339 timestamp)", true, false},
		{"from: events", "2026-01-01T00:00:00Z", "2026-02-02T00:00:00Z", nil,
			"the range from..to spans more than 31 days; query at most 31 days at a time", true, false},
		{"from: events", "2026-02-01T00:00:00Z", "2026-02-01T00:00:00Z", nil, "from must be before to", true, false},
	} {
		_, _, err := queryCall(t, ctx, db, c.q, c.from, c.to, c.limit)
		var ae *ArgError
		if err == nil || err.Error() != c.want || !errors.As(err, &ae) {
			t.Fatalf("%q: error %v, want %q", c.q, err, c.want)
		}
	}
	d, _, err := queryCall(t, ctx, db, "FROM: EVENTS | MEASURES: COUNT(*)", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", nil)
	if err != nil || queryRows(t, d) != "[[21]]" {
		t.Fatalf("relation names are case-insensitive: %v %v", d, err)
	}
}

func TestQueryBoundedScanAndPatternCap(t *testing.T) {
	db, _ := newQueryDB(t)
	if len(db.parts) < 2 {
		t.Fatalf("the store holds %d part(s), want several", len(db.parts))
	}
	ctx, cancel := context.WithCancel(context.Background())
	src, err := newQLSource(ctx, db, time.Unix(0, 0), time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	calls := 0
	err = src.Scan(ctx, ql.ScanRequest{Relation: "events", Columns: []ql.Column{{Name: "event_uid", Type: ql.TString}}},
		func(ql.Row) error { calls++; return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("the scan delivered %d row(s) after the context was cancelled", calls)
	}
	const want = "1:37: like pattern must be at most 1000 bytes"
	_, _, err = queryCall(t, context.Background(), db, "from: events | filter: text_content like '"+strings.Repeat("a", 1001)+"'",
		"2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", nil)
	if err == nil || err.Error() != want {
		t.Fatalf("over-long like pattern: error %v, want %q", err, want)
	}
}

func plantControl(t *testing.T, dir, author, line string) {
	t.Helper()
	src := []byte(line + "\n")
	enc, _ := zstd.NewWriter(nil)
	zst := enc.EncodeAll(src, nil)
	suffix := time.Now().UTC().Format("2006-01-02") + "-c1"
	d := filepath.Join(dir, "raw", "author="+author, "machine="+store.ServerMachine, "harness=control")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	m, _ := json.Marshal(map[string]any{"src_id": suffix, "path": "/c/" + suffix, "size": len(src), "updated_ms": 1, "zst_size": len(zst)})
	if err := os.WriteFile(filepath.Join(d, suffix+".json"), m, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, suffix+".jsonl.zst"), zst, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestQueryHidesAControlledSession(t *testing.T) {
	db, dir := newQueryDB(t)
	const sid = "7c666c01-d38e-4658-8650-854ffb5b626e"
	plantControl(t, dir, "b", `{"session_id":"`+sid+`","action":"delete","ts":"2026-10-23T00:00:00Z","request_key":"k1"}`)
	ctl, err := LoadControls(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ctl.deleted(sid) {
		t.Fatal("the control record did not load")
	}
	id, _ := auth.NewKeys(map[string]auth.Grant{"k-b": {Author: "b"}}).Identify("k-b")
	ctx := auth.NewContext(WithControls(context.Background(), ctl), id)
	d, _, err := queryCall(t, ctx, db, "from: sessions | measures: count()", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := queryRows(t, d); got != "[[1]]" {
		t.Fatalf("a deleted session is still a row: %s", got)
	}
}

func TestQueryHidesAPrivateSession(t *testing.T) {
	db, dir := newQueryDB(t)
	const sid = "7c666c01-d38e-4658-8650-854ffb5b626e"
	plantControl(t, dir, "a", `{"session_id":"`+sid+`","action":"visibility","visibility":"private","ts":"2026-10-23T00:00:00Z","request_key":"k1"}`)
	ctl, err := LoadControls(dir)
	if err != nil {
		t.Fatal(err)
	}
	keys := auth.NewKeys(map[string]auth.Grant{"k-a": {Author: "a"}, "k-b": {Author: "b"}})
	for _, c := range []struct{ key, want string }{{"k-a", "[[2]]"}, {"k-b", "[[1]]"}} {
		id, _ := keys.Identify(c.key)
		ctx := auth.NewContext(WithControls(context.Background(), ctl), id)
		d, _, err := queryCall(t, ctx, db, "from: sessions | measures: count()", "2026-01-16T00:00:00Z", "2026-02-16T00:00:00Z", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := queryRows(t, d); got != c.want {
			t.Fatalf("%s sees %s sessions, want %s", c.key, got, c.want)
		}
	}
}

func TestQuerySpanAndAnnotationRows(t *testing.T) {
	db, _ := newQueryDB(t)
	const sid = "7c666c01-d38e-4658-8650-854ffb5b626e"
	f, err := Sessions(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.25
	spans := []schema.Span{{
		SessionID: sid, SpanID: "sp1", Kind: schema.SpanChat, Name: "call", Status: schema.SpanOK,
		Start: time.Date(2026, 1, 19, 15, 58, 41, int(421*time.Millisecond), time.UTC),
		End:   time.Date(2026, 1, 19, 15, 58, 42, int(500*time.Millisecond), time.UTC),
		Model: "m1", InputTokens: 10, OutputTokens: 20, CacheReadTokens: 1, CacheWriteTokens: 2, Cost: &cost,
	}}
	annos := []schema.Annotation{{
		ID: "an1", Target: schema.Target{SessionID: sid}, Name: "quality",
		Value: schema.Number(0.5), Source: schema.SourceHuman,
		TS: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC), Author: "a",
	}}
	s := &qlSource{db: db, f: f, from: time.Unix(0, 0), to: time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC),
		spansOf: func(id string) []schema.Span {
			if id == sid {
				return spans
			}
			return nil
		},
		annotationsOf: func(id string) []schema.Annotation {
			if id == sid {
				return annos
			}
			return nil
		}}
	for _, c := range []struct{ name, q, want string }{
		{"spans", `from: spans | select: session_id, span_id, kind, name, start_time, end_time, status, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost | sort: span_id`,
			`[["7c666c01-d38e-4658-8650-854ffb5b626e","sp1","chat","call","2026-01-19T15:58:41.421Z","2026-01-19T15:58:42.5Z","ok","m1",10,20,1,2,0.25]]`},
		{"annotations", `from: annotations | filter: value_number > 0 | select: name, value_number, source, author, target_span_id | sort: name`,
			`[["quality",0.5,"human","a",null]]`},
	} {
		parsed, err := ql.Parse(c.q)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := ql.Check(parsed, ql.Relations)
		if err != nil {
			t.Fatal(err)
		}
		res, err := ql.Run(context.Background(), plan, s, ql.Limits{Rows: 100, Held: 1000})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := jsonOf(t, res.Rows); got != c.want {
			t.Fatalf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
