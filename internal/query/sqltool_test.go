package query_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/query"
)

// The lake has tenant a = alice (codex, Feb 15), tenant b = bob (claude-code,
// Jan 19) and carol (cursor, Feb 16 to May 8).
type lake struct {
	ix   *cache.Index
	with func(ctx context.Context, token string) context.Context
}

func newLake(t *testing.T) lake {
	t.Helper()
	dataDir := t.TempDir()
	for _, s := range []struct{ author, golden string }{
		{"alice", "codex"}, {"bob", "claude_code"}, {"carol", "cursor"}, {"carol", "cursor_sqlite"},
	} {
		if err := cachetest.Source(dataDir, layout.Key{Author: s.author}, s.golden); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := cache.Open(dataDir, cache.Options{MemoryLimit: "1GB", FlagsSQL: query.SearchFlagsSQL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	if _, err := ix.Converge(context.Background(), cache.RetryFailed); err != nil {
		t.Fatal(err)
	}
	ctl, err := query.LoadControls(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokens(map[string]auth.Token{
		auth.Hash("alice"): {Tenant: "a", Author: "alice"},
		auth.Hash("bob"):   {Tenant: "b", Author: "bob"},
		auth.Hash("carol"): {Tenant: "b", Author: "carol"},
	})
	return lake{ix: ix, with: func(ctx context.Context, token string) context.Context {
		ctx, ok := tokens.Context(query.WithControls(ctx, ctl), token)
		if !ok {
			t.Fatalf("token %s", token)
		}
		return ctx
	}}
}

func (l lake) call(ctx context.Context, token, args string) (map[string]any, error) {
	tl, _ := query.Lookup("query")
	data, _, err := tl.Call(l.with(ctx, token), l.ix.DB(), json.RawMessage(args))
	if err != nil {
		return nil, err
	}
	return data.(map[string]any), nil
}

const (
	jan = `"from":"2026-01-01T00:00:00Z","to":"2026-01-31T00:00:00Z"`
	feb = `"from":"2026-02-01T00:00:00Z","to":"2026-03-02T00:00:00Z"`
)

func args(sql, rng string) string {
	b, _ := json.Marshal(sql)
	return `{"sql":` + string(b) + `,` + rng + `}`
}

func withLimit(args, limit string) string {
	return strings.TrimSuffix(args, "}") + `,"limit":` + limit + "}"
}

func rowsJSON(t *testing.T, d map[string]any) string {
	t.Helper()
	b, err := json.Marshal(d["rows"])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestQueryResults(t *testing.T) {
	l := newLake(t)
	ctx := context.Background()
	for _, c := range []struct{ name, token, sql, rng, want string }{
		{"grouped count by harness", "alice", "SELECT harness, count(*) AS n FROM events GROUP BY harness ORDER BY harness", feb, `[["codex",15]]`},
		{"events of the other tenant's range", "bob", "SELECT harness, count(*) AS n FROM events GROUP BY harness ORDER BY harness", feb, `[["cursor",7]]`},
		{"a session overlapping the range is a sessions row", "bob", "SELECT count(*) FROM sessions", `"from":"2026-04-01T00:00:00Z","to":"2026-04-02T00:00:00Z"`, `[[1]]`},
		{"alice sees none of tenant b in January", "alice", "SELECT (SELECT count(*) FROM events), (SELECT count(*) FROM sessions), (SELECT count(*) FROM turns), (SELECT count(*) FROM spans)", jan, `[[0,0,0,0]]`},
		{"bob sees none of tenant a in February", "bob", "SELECT count(*) FROM events WHERE author = 'alice' OR harness = 'codex'", feb, `[[0]]`},
		{"bob sees his own in January", "bob", "SELECT harness, count(*) FROM events GROUP BY harness", jan, `[["claude-code",6]]`},
		{"order survives the row cap wrapper", "alice", "SELECT event_ts FROM events ORDER BY event_ts LIMIT 2", feb, `[["2026-02-15T03:50:40Z"],["2026-02-15T03:50:42.191Z"]]`},
		{"events outside the range are not rows", "alice", "SELECT count(*) FROM events", `"from":"2026-02-15T03:50:41Z","to":"2026-02-15T03:50:44Z"`, `[[2]]`},
		{"common functions, CTE, window and aggregate", "alice",
			`WITH t AS (SELECT event_kind, event_ts FROM events) SELECT strftime(date_trunc('day', min(event_ts)), '%Y-%m-%d') AS day, count(*) AS n, ` +
				`row_number() OVER (ORDER BY count(*) DESC) AS rn, list_aggregate([1,2,3], 'sum') AS s, regexp_extract('a1', '[0-9]') AS d, ` +
				`json_extract_string('{"a":"b"}', '$.a') AS j, struct_pack(a := 1) AS st, round(quantile_cont(epoch(event_ts), 0.5)) AS q ` +
				`FROM t GROUP BY event_kind ORDER BY n DESC, event_kind LIMIT 1`, feb, `[["2026-02-15",4,1,6,"1","b",{"a":1},1771127447]]`},
	} {
		d, err := l.call(ctx, c.token, args(c.sql, c.rng))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := rowsJSON(t, d); got != c.want {
			t.Fatalf("%s: rows %s, want %s", c.name, got, c.want)
		}
	}
}

func TestQueryRowCap(t *testing.T) {
	l := newLake(t)
	d, err := l.call(context.Background(), "alice", withLimit(args("SELECT event_uid FROM events", feb), "2"))
	if err != nil {
		t.Fatal(err)
	}
	if d["row_count"] != 2 || d["truncated"] != true || d["limit"] != 2 {
		t.Fatalf("limit 2: row_count %v truncated %v limit %v", d["row_count"], d["truncated"], d["limit"])
	}
	d, err = l.call(context.Background(), "alice", args("SELECT event_uid FROM events", feb))
	if err != nil {
		t.Fatal(err)
	}
	if d["row_count"] != 15 || d["truncated"] != false || d["limit"] != 100 {
		t.Fatalf("default limit: row_count %v truncated %v limit %v", d["row_count"], d["truncated"], d["limit"])
	}
	for _, limit := range []string{"0", "10001"} {
		_, err = l.call(context.Background(), "alice", withLimit(args("SELECT 1", feb), limit))
		if err == nil || err.Error() != "limit must be between 1 and 10000" {
			t.Fatalf("limit %s: %v", limit, err)
		}
	}
}

func TestQueryRefusals(t *testing.T) {
	l := newLake(t)
	const onlySelect = "only one SELECT statement is allowed"
	const views = "; query one of annotations, events, sessions, spans, turns"
	for _, c := range []struct{ sql, rng, want string }{
		{"DELETE FROM events", feb, onlySelect},
		{"CREATE TABLE x AS SELECT 1", feb, onlySelect},
		{"COPY events TO '/tmp/x.parquet'", feb, onlySelect},
		{"ATTACH '/tmp/x.db'", feb, onlySelect},
		{"PRAGMA database_list", feb, onlySelect},
		{"SET threads = 1", feb, onlySelect},
		{"SELECT * FROM read_parquet('/etc/x.parquet')", feb, `table function "read_parquet" is not allowed`},
		{"SELECT * FROM read_csv('/etc/passwd')", feb, `table function "read_csv" is not allowed`},
		{"SELECT * FROM glob('/*')", feb, `table function "glob" is not allowed`},
		{"SELECT * FROM query('SELECT 1')", feb, `table function "query" is not allowed`},
		{"SELECT getenv('HOME')", feb, `function "getenv" is not allowed`},
		{"SELECT levenshtein(a.text_content, b.text_content) FROM events a, events b", feb, `function "levenshtein" is not allowed`},
		{"SELECT pg_catalog.getenv('HOME')", feb, `function "pg_catalog.getenv" is not allowed`},
		{"SELECT (SELECT count(*) FROM read_parquet('x'))", feb, `table function "read_parquet" is not allowed`},
		{"SELECT * FROM main.events", feb, `table "main.events" must be named without a schema` + views},
		{"SELECT * FROM information_schema.tables", feb, `table "information_schema.tables" must be named without a schema` + views},
		{"SELECT * FROM lake_files", feb, `table "lake_files" is not queryable` + views},
		{"SELECT * FROM 'x.parquet'", feb, `table "x.parquet" is not queryable` + views},
		{"SELECT * FROM events WHERE session_id IN (SELECT session_id FROM main.events)", feb, `table "main.events" must be named without a schema` + views},
		{"WITH lake_files AS (SELECT * FROM lake_files) SELECT * FROM lake_files", feb, `table "lake_files" is not queryable` + views},
		{"SELECT * FROM (WITH lake_files AS (SELECT 1) SELECT * FROM lake_files), lake_files", feb, `table "lake_files" is not queryable` + views},
		{"WITH RECURSIVE r AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM r WHERE n < 3) SELECT * FROM r", feb, "recursive CTEs are not allowed"},
		{"SELECT $1", feb, "query parameters are not allowed; write the value into sql"},
		{"SELECT 1; SELECT 2", feb, "exactly one statement is allowed, got 2"},
		{"", feb, "sql is required"},
		{"SELECT 1", `"to":"2026-03-02T00:00:00Z"`, "from is required (an RFC 3339 timestamp)"},
		{"SELECT 1", `"from":"2026-02-01T00:00:00Z"`, "to is required (an RFC 3339 timestamp)"},
		{"SELECT 1", `"from":"2026-01-01T00:00:00Z","to":"2026-02-02T00:00:00Z"`, "the range from..to spans more than 31 days; query at most 31 days at a time"},
		{"SELECT 1", `"from":"2026-02-01T00:00:00Z","to":"2026-02-01T00:00:00Z"`, "from must be before to"},
	} {
		_, err := l.call(context.Background(), "alice", args(c.sql, c.rng))
		var ae *query.ArgError
		if err == nil || err.Error() != c.want || !errors.As(err, &ae) {
			t.Fatalf("%q: error %v, want %q", c.sql, err, c.want)
		}
	}
	d, err := l.call(context.Background(), "alice", args("SELECT count(*) FROM EVENTS", feb))
	if err != nil || rowsJSON(t, d) != "[[15]]" {
		t.Fatalf("view names are case-insensitive: %v %v", d, err)
	}
}

const crossJoin = "SELECT count(*) FROM events a, events b, events c, events d, events e, events f, events g, events h, events i, events j, events k, events l, events m, events n"

func TestQueryTimeout(t *testing.T) {
	l := newLake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := l.call(ctx, "alice", args(crossJoin, feb))
	if err == nil || err.Error() != "statement timeout: the query was stopped at its time limit of 30 s" {
		t.Fatalf("error %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the statement ran %s after a 300 ms deadline", took)
	}
	if d, err := l.call(context.Background(), "alice", args("SELECT count(*) FROM events", feb)); err != nil || rowsJSON(t, d) != "[[15]]" {
		t.Fatalf("after a timeout: %v %v", d, err)
	}
}
