package ql_test

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/ql"
	"github.com/candyflipline/mnemonic/internal/schema"
)

type memSource struct {
	rels      map[string][]map[string]ql.Value
	heavyGets int
	lastHints []ql.MatchHint
}

type memRow struct {
	src  *memSource
	req  ql.ScanRequest
	vals map[string]ql.Value
}

func (r memRow) Get(i int) (ql.Value, error) {
	c := r.req.Columns[i]
	if c.Heavy {
		r.src.heavyGets++
	}
	return r.vals[c.Name], nil
}

func (s *memSource) Scan(ctx context.Context, req ql.ScanRequest, fn func(ql.Row) error) error {
	s.lastHints = req.Match
	for _, vals := range s.rels[req.Relation] {
		if err := fn(memRow{src: s, req: req, vals: vals}); err != nil {
			return err
		}
	}
	return nil
}

func ts(s string) ql.Value {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return ql.TimeValue(t)
}

type row = map[string]ql.Value

var (
	str  = ql.StringValue
	num  = ql.IntValue
	flt  = ql.FloatValue
	strs = ql.StringsValue
)

func event(uid, session, harness, author, at, kind, tool, text string, tokens int64, latency ql.Value, types ql.Value, buckets ql.Value, payload string) row {
	return row{"event_uid": str(uid), "session_id": str(session), "harness": str(harness), "author": str(author),
		"event_ts": ts(at), "event_kind": str(kind), "tool_name": str(tool), "text_content": str(text),
		"input_tokens": num(tokens), "latency_ms": latency, "content_types": types, "token_usage_buckets": buckets,
		"payload_json": str(payload)}
}

func data() *memSource {
	return &memSource{rels: map[string][]map[string]ql.Value{
		"events": {
			event("e1", "s1", "codex", "alice", "2026-02-15T03:50:40Z", "message", "", "Hello world from codex", 10, num(100), strs([]string{"text"}), ql.IntMapValue(map[string]int64{"input": 10}), `{"a":1}`),
			event("e2", "s1", "codex", "alice", "2026-02-15T03:50:42.191Z", "tool_call", "Bash", "ls -la", 20, num(200), strs([]string{"text", "image"}), ql.IntMapValue(map[string]int64{"input": 20, "output": 5}), `{"cmd":"rm -rf /tmp/x"}`),
			event("e3", "s1", "codex", "alice", "2026-02-15T03:50:43Z", "tool_call", "Bash", "git status", 30, num(300), strs([]string{"text"}), ql.Null(), `{"cmd":"git status"}`),
			event("e4", "s2", "codex", "alice", "2026-02-16T10:00:00Z", "message", "", "world hello again", 40, num(400), strs([]string{"image"}), ql.IntMapValue(map[string]int64{"input": 40}), `{}`),
			event("e5", "s3", "claude-code", "bob", "2026-02-16T11:00:00Z", "tool_call", "Read", "read the world file", 50, ql.Null(), ql.Null(), ql.Null(), `{}`),
			event("e6", "s3", "claude-code", "bob", "2026-02-17T12:00:00Z", "message", "", "goodbye", 60, num(600), strs([]string{"text"}), ql.Null(), `{}`),
		},
		"sessions": {
			{"session_id": str("s1"), "total_turns": num(3), "first_event_time": ts("2026-02-15T03:50:40Z")},
			{"session_id": str("s2"), "total_turns": num(1), "first_event_time": ts("2026-02-16T10:00:00Z")},
			{"session_id": str("s3")},
		},
		"turns": {
			{"session_id": str("s1"), "turn_seq": num(0), "event_count": num(4), "completed": num(1), "terminal_uid": str("e1")},
			{"session_id": str("s1"), "turn_seq": num(2), "event_count": num(1), "completed": num(0), "terminal_uid": str("e3")},
			{"session_id": str("s1"), "turn_seq": num(1), "event_count": num(2), "completed": num(1), "terminal_uid": str("e2")},
		},
		"spans": {
			{"span_id": str("sp2"), "cost": ql.Null()},
			{"span_id": str("sp1"), "cost": flt(0.25)},
		},
		"annotations": {
			{"name": str("quality"), "value_number": flt(0.5)},
			{"name": str("verdict"), "value_number": ql.Null(), "value_label": str("good")},
		},
	}}
}

func TestQL(t *testing.T) {
	const views = "; query one of annotations, events, sessions, spans, turns"
	const sql = "SQL is not accepted; write clauses, for example: from: events | filter: harness = 'codex' | select: session_id, event_ts"
	for _, c := range []struct {
		name, q    string
		rows, held int
		cancel     bool
		want, err  string
		truncated  bool
		cols       string
		heavy      int
		hints      string
	}{
		{name: "grouped count by harness", q: "from: events | dimensions: harness | measures: count() as n | sort: harness",
			want: `[["claude-code",2],["codex",4]]`, cols: `[{"name":"harness","type":"string"},{"name":"n","type":"int"}]`},
		{name: "sort by a column the select drops, then limit", q: "from: events | select: event_uid, input_tokens * 2 as dbl | sort: input_tokens desc | limit: 2",
			want: `[["e6",120],["e5",100]]`, heavy: 6},
		{name: "order survives the row cap", q: "from: events\nselect: event_ts\nsort: event_ts\nlimit: 2",
			want: `[["2026-02-15T03:50:40Z"],["2026-02-15T03:50:42.191Z"]]`},
		{name: "a global aggregate over no rows is one row", q: "from: sessions | filter: total_turns > 100 | measures: count(), sum(total_turns) as t",
			want: `[[0,null]]`, cols: `[{"name":"count()","type":"int"},{"name":"t","type":"int"}]`},
		{name: "names and keywords are case-insensitive", q: "FROM: EVENTS | FILTER: author = 'bob' OR tool_name = 'Bash' | MEASURES: COUNT(*) AS n",
			want: `[[4]]`},
		{name: "a string literal against a time column is a time", q: "from: events | filter: event_ts >= '2026-02-15T03:50:41Z' and event_ts < '2026-02-16' | select: event_uid",
			want: `[["e2"],["e3"]]`},
		{name: "match is handed to the source and rechecked", q: "from: events | filter: text_content match 'Hello, World' | select: event_uid",
			want: `[["e1"],["e4"]]`, hints: `[{"Column":"text_content","Words":["hello","world"]}]`},
		{name: "a heavy column is read only after the cheap conjunct passed", q: "from: events | filter: payload_json like '%rm -rf%' and tool_name = 'Bash' | select: event_uid",
			want: `[["e2"]]`, heavy: 2},
		{name: "day dimension and every aggregate", q: "from: events | filter: harness = 'codex' | dimensions: day(event_ts) as day | measures: count() as n, sum(input_tokens) as tin, avg(latency_ms) as lat, percentile(latency_ms, 0.5) as p50, count_distinct(session_id) as sessions, max(event_ts) as last | sort: day",
			want: `[["2026-02-15T00:00:00Z",3,60,200,200,1,"2026-02-15T03:50:43Z"],["2026-02-16T00:00:00Z",1,40,400,400,1,"2026-02-16T10:00:00Z"]]`, heavy: 8},
		{name: "includes and a map key that is missing", q: "from: events | filter: content_types includes 'image' | select: event_uid, token_usage_buckets['output'] as out",
			want: `[["e2",5],["e4",null]]`},
		{name: "is null or not in", q: "from: events | filter: latency_ms is null or tool_name not in ('Bash', '') | select: event_uid",
			want: `[["e5"]]`, heavy: 6},
		{name: "ilike, lower and length", q: "from: events | filter: text_content ilike 'HELLO%' | select: lower(harness) as h, length(text_content) as len",
			want: `[["codex",22]]`},
		{name: "week, month and hour", q: "from: events | filter: event_uid = 'e6' | select: week(event_ts) as w, month(event_ts) as m, hour(event_ts) as h",
			want: `[["2026-02-16T00:00:00Z","2026-02-01T00:00:00Z","2026-02-17T12:00:00Z"]]`},
		{name: "arithmetic over a null is null", q: "from: spans | select: span_id, cost * 100 as cents | sort: span_id",
			want: `[["sp1",25],["sp2",null]]`},
		{name: "a float column against an int literal", q: "from: annotations | filter: value_number > 0 | select: name, value_number",
			want: `[["quality",0.5]]`},
		{name: "the row cap truncates", q: "from: turns | select: session_id, turn_seq | sort: turn_seq", rows: 2,
			want: `[["s1",0],["s1",1]]`, truncated: true},
		{name: "no select is select *", q: "from: turns | limit: 1",
			want: `[["s1",0,4,1,"e1"]]`},
		{name: "SQL", q: "SELECT count(*) FROM events", err: "1:1: " + sql},
		{name: "SQL statement", q: "DELETE FROM events", err: "1:1: " + sql},
		{name: "relation off the list", q: "from: lake_files", err: `1:7: relation "lake_files" is not queryable` + views},
		{name: "function off the list", q: "from: events | select: getenv('HOME')",
			err: `1:24: function "getenv" is not allowed; the functions are lower, length, hour, day, week, month, and in measures: count, count_distinct, sum, avg, min, max, percentile`},
		{name: "unknown column", q: "from: events | select: nope", err: `1:24: events has no column "nope"`},
		{name: "type mismatch", q: "from: events | filter: harness = 1", err: "1:32: cannot compare string with int"},
		{name: "filter not a predicate", q: "from: events | filter: harness", err: "1:24: filter must be true or false, got string"},
		{name: "aggregate in select", q: "from: events | select: count()", err: "1:24: count is an aggregate; it belongs in measures:"},
		{name: "rows and groups mixed", q: "from: events | select: event_uid | measures: count()",
			err: "1:36: select returns rows and dimensions/measures return groups; use one or the other"},
		{name: "clause twice", q: "from: events | from: spans", err: "1:16: the from clause is given twice (first at 1:1)"},
		{name: "no from", q: "select: event_uid", err: "1:1: a query needs a from: clause naming the relation to read"},
		{name: "open string", q: "from: events | filter: harness = 'codex", err: "1:34: the string starting here has no closing '"},
		{name: "not a time", q: "from: events | filter: event_ts > 'yesterday'",
			err: `1:35: "yesterday" is not a time; write RFC 3339, for example '2026-02-15T03:50:41Z'`},
		{name: "limit zero", q: "from: events | limit: 0", err: `1:23: limit takes a positive whole number, got "0"`},
		{name: "grouped sort off the output", q: "from: events | dimensions: harness | measures: count() as n | sort: tool_name",
			err: "1:69: a grouped query sorts by its output columns: harness, n"},
		{name: "match with no words", q: "from: events | filter: text_content match '!!'", err: "1:37: match needs at least one word"},
		{name: "two expressions without a comma", q: "from: events | select: event_uid event_kind", err: `1:34: unexpected "event_kind" in the select clause`},
		{name: "SQL keyword inside a clause", q: "from: events where harness = 'x'", err: `1:14: unexpected "where" in the from clause`},
		{name: "groups past the memory bound", q: "from: events | dimensions: event_uid | measures: count()", held: 3,
			err: "the query holds more than 3 groups and values in memory; add a filter or group by fewer dimensions"},
		{name: "cancelled", q: "from: events | measures: count()", cancel: true, err: "context canceled"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := data()
			ctx, cancel := context.WithCancel(context.Background())
			if c.cancel {
				cancel()
			} else {
				defer cancel()
			}
			rows, held := c.rows, c.held
			if rows == 0 {
				rows = 100
			}
			if held == 0 {
				held = 1000
			}
			res, err := run(ctx, c.q, src, ql.Limits{Rows: rows, Held: held})
			if c.err != "" {
				if err == nil || err.Error() != c.err {
					t.Fatalf("error %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := jsonOf(t, res.Rows); got != c.want {
				t.Fatalf("rows %s, want %s", got, c.want)
			}
			if res.Truncated != c.truncated {
				t.Fatalf("truncated %v, want %v", res.Truncated, c.truncated)
			}
			if c.cols != "" {
				if got := jsonOf(t, res.Columns); got != c.cols {
					t.Fatalf("columns %s, want %s", got, c.cols)
				}
			}
			if src.heavyGets != c.heavy {
				t.Fatalf("heavy column reads %d, want %d", src.heavyGets, c.heavy)
			}
			if c.hints != "" {
				if got := jsonOf(t, src.lastHints); got != c.hints {
					t.Fatalf("match hints %s, want %s", got, c.hints)
				}
			}
		})
	}
}

func run(ctx context.Context, q string, src ql.RowSource, lim ql.Limits) (ql.Result, error) {
	parsed, err := ql.Parse(q)
	if err != nil {
		return ql.Result{}, err
	}
	p, err := ql.Check(parsed, ql.Relations)
	if err != nil {
		return ql.Result{}, err
	}
	return ql.Run(ctx, p, src, lim)
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var ddlColumn = regexp.MustCompile(`(?m)^\s+(\w+) (\w+)`)

var ddlTypes = map[string]ql.Type{"VARCHAR": ql.TString, "ENUM": ql.TString, "TIMESTAMP": ql.TTime, "UBIGINT": ql.TInt, "DOUBLE": ql.TFloat}

func TestSchemaMatchesOwners(t *testing.T) {
	for _, owner := range []struct {
		rel, ddl string
		hidden   []string
	}{
		{"spans", schema.SpansDDL, nil},
		{"annotations", schema.AnnotationsDDL, []string{"lake_file", "lake_row"}},
	} {
		var want []string
		for _, m := range ddlColumn.FindAllStringSubmatch(owner.ddl, -1) {
			if !contains(owner.hidden, m[1]) {
				want = append(want, m[1]+" "+ddlTypes[m[2]].String())
			}
		}
		var got []string
		for _, r := range ql.Relations {
			if r.Name == owner.rel {
				for _, c := range r.Columns {
					got = append(got, c.Name+" "+c.Type.String())
				}
			}
		}
		if strings.Join(got, ", ") != strings.Join(want, ", ") {
			t.Fatalf("%s in internal/ql/schema.go is\n  %s\nbut its owner internal/schema declares\n  %s", owner.rel, strings.Join(got, ", "), strings.Join(want, ", "))
		}
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
