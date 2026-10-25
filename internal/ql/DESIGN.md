# ql: the query language of the `query` tool

The `query` tool answers one read-only question over five relations (events,
sessions, turns, spans, annotations). Until now the question was SQL run by
DuckDB behind a guard (`internal/query/sqltool.go`, `internal/query/sqlguard.go`).
The owner's call (Oct 23): keep the tool, drop DuckDB and the SQL structure,
make it Braintrust BTQL-shaped. `internal/ql` is that language: a grammar, a
typed AST parsed once at the tool boundary, a checker against the schema
(relations and columns as data), and an evaluator over a small row-source
interface the engine implements.

## Lies table

| Lie underneath | The one fix | Deleted |
|---|---|---|
| `query` means SQL, so it needs a SQL engine | a clause language with no statements, no table names beyond five, no subqueries, parsed once into a typed AST and run over engine rows | the SQL surface, `sqlguard.go` and its 400-name function allowlist, DuckDB's parser as a security boundary |
| Safety comes from walking an attacker's syntax tree | nothing unsafe is expressible: the grammar has no statement other than a query, `from:` takes one of five names, functions are a closed list of thirteen | every refusal test that guarded a SQL escape (they become parse errors by construction) |
| A filter must decode the whole row | rows are lazy: the evaluator asks for a column only when a predicate needs it, cheap conjuncts first, heavy columns (payload_json, token_usage_json) last | full-row materialization per event |
| Full text needs a scan | `match` is an AST node the checker lifts into the scan request, so the source can drive it from postings; the evaluator re-checks every row | nothing (the recheck keeps a source that ignores the hint correct) |

## Candidates

The same agent question in each: "codex tool calls per tool in February, most
first, top 10".

**A. BTQL clauses.** One clause per concern, each named, in any order, separated
by `|` or a newline.

    from: events
    | filter: harness = 'codex' and tool_phase = 'request'
    | dimensions: tool_name
    | measures: count() as calls
    | sort: calls desc
    | limit: 10

**B. A JSON object.** The tool's `query` argument is an object; the expression
language is nested arrays.

    {"from": "events",
     "filter": {"and": [["=", "harness", "codex"], ["=", "tool_phase", "request"]]},
     "dimensions": ["tool_name"],
     "measures": [{"count": [], "as": "calls"}],
     "sort": [["calls", "desc"]], "limit": 10}

**C. A pipeline (KQL / PRQL style).** Stages applied left to right; each stage
reads the previous one's output.

    events
    | where harness == 'codex' and tool_phase == 'request'
    | summarize calls = count() by tool_name
    | sort by calls desc
    | take 10

| | A. BTQL clauses | B. JSON object | C. Pipeline |
|---|---|---|---|
| What an agent writes | short, close to the SQL it knows; BTQL is documented publicly | 2-3x longer; easy to nest a filter wrong; JSON schema can describe the clause keys but not the expressions | short; agents know KQL less well than SQL; `==` vs `=` trips them |
| Parser | ~600 lines, hand-written recursive descent | none for clauses (encoding/json), but an expression decoder over `[]any` that must refuse every shape by hand: the same work, worse errors (no line:col) | as A, plus stage typing: each stage's output schema is the next stage's input |
| Semantics | fixed order: from, filter, group, sort, limit; one shape to evaluate | as A | open-ended: filter after summarize, two summarizes, sort before filter; the evaluator becomes a general relational engine |
| Errors | `line:col: message` on the source text | a JSON path, no position in what the agent wrote | as A |
| Cost bound | known before the scan: at most one grouping, one sort, a top-k heap | as A | each stage needs its own bound; a summarize over a summarize holds two group tables |
| Pushdown | filter is one expression over one relation: conjuncts, `match` hints and needed columns fall out of the AST | as A | only stages before the first summarize push down; the rest is in memory |
| SQL leakage | none: no statement keyword, no `FROM t1, t2`, no subquery | none | none |

**Pick: A.** It is what the owner named (Braintrust BTQL), it is the shortest
thing an agent can write correctly on the first try, and its fixed clause order
makes the evaluator one pass with a bounded state (filter, then one group table
or one top-k heap, then the cut). B moves the parse into hand-checked `[]any`
shapes with worse errors and longer queries for no safety gain, because A has
no unsafe construct either. C buys composability (filter after aggregate, two
aggregation levels) that no test in `sqltool_test.go` and no listed agent
question needs, and pays for it with an evaluator that is a general relational
engine. If having-style filters are ever needed, BTQL's own answer is one more
clause (`final_filter:`), not a pipeline.

## Grammar

Keywords, clause names, relation and column names are case-insensitive
(lowercased by the lexer); string literals keep their case. Whitespace,
newlines included, separates tokens only. A clause starts at a name followed
by `:`, which appears nowhere else, so `|` between clauses is optional and a
newline works as a separator.

```ebnf
query      = clause { [ "|" ] clause } .
clause     = "from" ":" name
           | "select" ":" ( "*" | item { "," item } )
           | "filter" ":" expr
           | "dimensions" ":" item { "," item }
           | "measures" ":" item { "," item }
           | "sort" ":" key { "," key }
           | "limit" ":" int .
item       = expr [ "as" name ] .
key        = expr [ "asc" | "desc" ] .
expr       = conj { "or" conj } .
conj       = neg { "and" neg } .
neg        = "not" neg | pred .
pred       = sum [ cmp sum
                 | [ "not" ] "in" "(" literal { "," literal } ")"
                 | [ "not" ] ( "like" | "ilike" ) string
                 | "match" string               (* left side: a column *)
                 | "includes" string
                 | "is" [ "not" ] "null" ] .
cmp        = "=" | "!=" | "<>" | "<" | "<=" | ">" | ">=" .
sum        = product { ( "+" | "-" ) product } .
product    = unary { ( "*" | "/" ) unary } .
unary      = "-" unary | postfix .
postfix    = primary [ "[" string "]" ] .           (* left side: a map column *)
primary    = literal | name | name "(" [ "*" | expr { "," expr } ] ")" | "(" expr ")" .
literal    = string | int | float | "true" | "false" | "-" ( int | float ) .
name       = letter_or_underscore { letter_or_underscore | digit } .   (* ASCII, not a reserved word *)
string     = "'" { any character but "'" | "''" } "'" .
```

Reserved words: and, or, not, in, is, null, like, ilike, match, includes, as,
asc, desc, true, false. No column of the five relations uses one.

Rules the grammar does not show:
- Each clause at most once, in any order; the meaning is fixed: from, filter,
  then rows (select) or groups (dimensions, measures), then sort, then limit.
- `from:` is required. No `select:`, `dimensions:` or `measures:` means
  `select: *`. `select:` and `dimensions:`/`measures:` do not mix.
- `null` is written only in `is null` / `is not null`.
- An output column is named by `as`, else by its column name, else by its
  source text (`count()`, `sum(input_tokens)`). Two outputs with one name are
  refused.

## Types

| ql type | Comes from | Literal |
|---|---|---|
| string | String, LowCardinality(String), VARCHAR, ENUM | `'text'`, `''` escapes a quote |
| int | UInt8..UInt64, INTEGER, BIGINT, UBIGINT | `42`, `-1` |
| float | Float64, DOUBLE | `0.5`, `1e3` |
| bool | predicates only (no column has it) | `true`, `false` |
| time | DateTime64(3), Date, TIMESTAMP (microseconds, UTC) | a string compared with a time: RFC 3339, `2006-01-02 15:04:05[.ffffff]`, or `2006-01-02`, read once at check time |
| list\<string\> | Array(String) (content_types) | none; `includes 'x'` |
| map\<string,int\>, map\<string,float\> | token_usage_buckets, token_usage_native_units | none; `col['key']`, null when the key is missing |

Every column may be null. Operators and their types:

| Operator | Operands | Result |
|---|---|---|
| `= != < <= > >=` | two of one scalar type, or int with float | bool |
| `and or not` | bool, three-valued (null is unknown) | bool |
| `+ - *` | numbers | int if both int, else float |
| `/` | numbers | float; null on division by zero |
| `- x` | number | its type |
| `in (...)`, `not in (...)` | a scalar and literals of its type | bool |
| `like`, `ilike`, `not like`, `not ilike` | string, pattern of `%` and `_` (no escape) | bool; ilike folds case |
| `match` | a string column and words | bool: every word of the literal is a word of the column (words are lowercase runs of letters and digits) |
| `includes` | list\<string\> and a string | bool |
| `is null`, `is not null` | any | bool, never null |
| `col['key']` | a map column | int or float |

`filter:` keeps a row only where it is true (not false, not null). Sort puts
nulls last in both directions; strings sort by bytes. Any operator with a null
operand is null, except `and`/`or` short-circuits and `is [not] null`.

## Functions

Thirteen, each earning its place against a query in `sqltool_test.go` or one an
agent plausibly asks. Everything else (window, list, map, struct, JSON,
regexp, strftime, epoch, set operations, CTEs, joins, subqueries) is gone.

| Function | Where | Earns its place against |
|---|---|---|
| `count()`, `count(*)`, `count(x)` | measures | every count in TestQueryResults; count(x) counts non-null |
| `count_distinct(x)` | measures | "how many sessions called Bash this week" |
| `sum(x)` | measures | "tokens per model"; int stays int, null over no rows |
| `avg(x)` | measures | "mean latency per model" |
| `min(x)`, `max(x)` | measures | `min(event_ts)` in the "common functions" test; "last activity per harness" |
| `percentile(x, p)` | measures | `quantile_cont(..., 0.5)` in the same test; "p95 latency"; linear interpolation like quantile_cont, p a literal in [0, 1] |
| `hour(t)`, `day(t)`, `week(t)`, `month(t)` | anywhere | `date_trunc('day', ...)` in the same test; "events per day". UTC; week starts Monday |
| `lower(s)` | anywhere | `dimensions: lower(tool_name)` across harnesses that case tool names differently |
| `length(s)` | anywhere | "longest assistant messages": `sort: length(text_content) desc`; counts characters |

An aggregate outside `measures:` and a non-aggregate inside it are refused. A
measure is one aggregate call; aggregates do not nest.

## Errors

Parse and check errors are `*ql.Error`, printed `line:col: message` (columns
count characters). A limit error has no position. The tool returns both as
its argument error. The messages (most are pinned in `ql_test.go`; abbreviated where marked ...):

- `SQL is not accepted; write clauses, for example: from: events | filter: harness = 'codex' | select: session_id, event_ts` (the first word is a SQL statement keyword)
- `relation "lake_files" is not queryable; query one of annotations, events, sessions, spans, turns`
- `function "getenv" is not allowed; the functions are lower, length, hour, day, week, month, and in measures: count, count_distinct, sum, avg, min, max, percentile`
- `events has no column "nope"`
- `cannot compare string with int`; `filter must be true or false, got string`
- `count is an aggregate; it belongs in measures:`; `measures takes aggregates (count, ...); put harness in dimensions`
- `select returns rows and dimensions/measures return groups; use one or the other`
- `the from clause is given twice (first at 1:1)`; `a query needs a from: clause naming the relation to read`
- `the string starting here has no closing '`; `strings use single quotes: 'text'`; `unexpected character '.'`
- `"yesterday" is not a time; write RFC 3339, for example '2026-02-15T03:50:41Z'`
- `limit takes a positive whole number, got "0"`
- `a grouped query sorts by its output columns: harness, n`
- `match needs at least one word`
- `unexpected "where" in the from clause`
- `the query holds more than N groups and values in memory; add a filter or group by fewer dimensions`

## Limits

| Bound | Where | Value |
|---|---|---|
| query length | tool | 20000 characters (today's `sqlMaxStatement`) |
| from..to | tool | required, at most 31 days, scopes every relation as today |
| rows returned | `Limits.Rows` = the tool's `limit` | default 100, at most 10000; `truncated` when more matched |
| time | the tool's context deadline | 30 s; Run checks the context every 256 rows and returns `ctx.Err()`; the engine source's scan checks it before each part and every 4096 rows it walks, so a cancelled or expired query stops without decoding the rest of the store; the tool keeps its `statement timeout` message (a cancelled context is reported the same way) |
| scan | the engine source | a query reads every part of the store: part metadata holds no event time bounds, so from..to filters rows, not parts; the cost bound is the 30 s deadline and the 31-day range (pruning parts by time is a format change, listed as a follow-up) |
| like/ilike pattern | `Check` | at most 1000 bytes, else `like pattern must be at most 1000 bytes` at the pattern's position; the matcher is a Shift-And automaton, one pass over the string with no backtracking |
| memory | `Limits.Held` | groups + distinct values + percentile values a grouped query holds; proposed 1,000,000 |
| concurrency | tool | two slots, `BusyError`, HTTP 429 with Retry-After 1, unchanged |

A row query holds at most `min(limit:, Limits.Rows + 1)` rows: unsorted it
stops the scan there; sorted it keeps a top-k heap of that size. So the only
query whose memory grows with the data is a grouped one, and `Held` bounds it.

## The row source

The binding implements one interface (`run.go`):

```go
type RowSource interface {
	Scan(ctx context.Context, req ScanRequest, fn func(Row) error) error
}
type ScanRequest struct { Relation string; Columns []Column; Match []MatchHint }
type Row interface { Get(i int) (Value, error) }
```

The contract:
1. `Scan` calls `fn` once per row of `req.Relation` that the caller may read
   and that lies in from..to. Visibility, author scope and the range are the
   source's (the tool's), never the query's: no clause can name or widen them.
2. When `fn` returns an error, `Scan` stops and returns that error unchanged
   (the evaluator stops early with it). Rows come in any order.
3. `Row.Get(i)` returns column `req.Columns[i]`, null or a value of the
   column's type; anything else fails the query naming the column. The row is
   valid only during `fn`. The evaluator calls `Get` at most once per column
   per row, and only when a predicate or an output needs it.
4. `req.Columns` lists only the columns the query reads. A column marked
   `Heavy` (payload_json, token_usage_json) is read only after every cheap
   conjunct of the filter passed: conjuncts are split at top-level `and` and
   ordered cheap first, so `filter: payload_json like '%rm%' and tool_name =
   'Bash'` decodes payload frames for Bash rows only (pinned by the test).
5. `req.Match` lists every `match` that is a top-level conjunct. A source may
   skip any row whose column lacks one of the hint's words, for example by
   intersecting postings, if its candidates are a superset of the rows where
   `words(column)` holds every hint word. The evaluator rechecks each row, so
   a source that ignores the hints is still correct.
6. Values: `IntValue(int64)` for every unsigned column (inode, offset,
   version fit in int64; a value that does not is the binding's error),
   `TimeValue` for DateTime64 and Date, `StringsValue`, `IntMapValue`,
   `FloatMapValue`.

The tool then reads `Result{Columns, Rows, Truncated}`. `Value` marshals as
JSON the way `jsonValue` does today: times as RFC 3339 UTC with fractional
seconds, ints as numbers, NaN and Inf as strings. Column types are ql's names
(`string`, `int`, `time`, ...), no longer DuckDB's (`VARCHAR`, `BIGINT`).

## The schema and how it stays in sync

`Relations` in `schema.go` is the one table.
- **events** is derived at package init from `schema.EventColumns`, typed from
  each column's ClickHouse type (the generated contract). A ClickHouse type
  with no ql mapping panics at init with a message naming
  `clickHouseTypes` in `internal/ql/schema.go`, so the first test of any
  package that links ql fails. After those come the columns the store derives
  per event and today's `events` view shows (author, machine_id, ingested_at,
  tool_io_*, event_time, event_order, turn_seq, event_ordinal). The cache's
  internal columns (doc, row_hash, flags, ...) are not in it, as today.
- **spans** and **annotations** are declared here and pinned by
  `TestSchemaMatchesOwners` against `schema.SpansDDL` and
  `schema.AnnotationsDDL` (name and type, in order; annotations' lake_file and
  lake_row are hidden as today). The test fails printing both lists.
- **sessions** and **turns** are declared here. Their owner today is
  `internal/cache/index.go:199,201`, which this feature deletes and ql may
  not import. The engine's `internal/index` becomes the owner; the binding
  lane either points the same check at what the engine declares or moves the
  declaration there and has ql read it.

## Every query of `sqltool_test.go` in ql

Tool-level arguments (from, to, limit) and their errors are unchanged; the
`sql` argument becomes `query`.

| Test / case | SQL | ql | Result |
|---|---|---|---|
| Results: grouped count by harness | `SELECT harness, count(*) AS n FROM events GROUP BY harness ORDER BY harness` | `from: events \| dimensions: harness \| measures: count() as n \| sort: harness` | same rows |
| Results: events of the other tenant's range | same as above (bob) | same as above | same rows |
| Results: a session overlapping the range is a sessions row | `SELECT count(*) FROM sessions` | `from: sessions \| measures: count()` | same |
| Results: alice sees none of tenant b in January | four scalar subqueries in one SELECT | four queries, `from: events \| measures: count()` and the same for sessions, turns, spans | not expressible in one query: no subqueries. Each scope is still checked, one per call |
| Results: bob sees none of tenant a | `... WHERE author = 'alice' OR harness = 'codex'` | `from: events \| filter: author = 'alice' or harness = 'codex' \| measures: count()` | same |
| Results: bob sees his own in January | `SELECT harness, count(*) FROM events GROUP BY harness` | `from: events \| dimensions: harness \| measures: count()` | same |
| Results: order survives the row cap wrapper | `SELECT event_ts FROM events ORDER BY event_ts LIMIT 2` | `from: events \| select: event_ts \| sort: event_ts \| limit: 2` | same (pinned in ql_test) |
| Results: events outside the range are not rows | `SELECT count(*) FROM events` | `from: events \| measures: count()` | same |
| Results: common functions, CTE, window and aggregate | CTE, strftime(date_trunc), row_number() OVER, list_aggregate, regexp_extract, json_extract_string, struct_pack, quantile_cont(epoch) | `from: events \| dimensions: event_kind \| measures: count() as n, min(event_ts) as first \| sort: n desc, event_kind \| limit: 1` | partly expressible: kind, n and first. Not expressible: the window (sort + limit gives rank 1), list/regexp/json/struct functions over literals (constants, not questions about the data), a percentile of a time (percentile takes numbers), a day string of an aggregate (functions apply to rows, not to measures) |
| RowCap: limit 2 / default / 0 / 10001 | `SELECT event_uid FROM events` | `from: events \| select: event_uid` | same: tool `limit` is `Limits.Rows` (pinned: "the row cap truncates") |
| Refusals: DELETE, CREATE TABLE, COPY, ATTACH, PRAGMA, SET | statements | none exists | kept by construction: `SQL is not accepted; ...` |
| Refusals: read_parquet, read_csv, glob, query table functions | `FROM f(...)` | `from:` takes a name | kept by construction: `unexpected "(" in the from clause` |
| Refusals: getenv, levenshtein | scalar calls | closed list of thirteen | kept: `function "getenv" is not allowed; ...` |
| Refusals: pg_catalog.getenv, main.events, information_schema.tables | qualified names | no qualified names | kept by construction: `unexpected character '.'` |
| Refusals: read_parquet in a subquery, CTE named lake_files, CTE in a subquery, recursive CTE | subqueries and CTEs | none exist | kept by construction |
| Refusals: lake_files | `FROM lake_files` | `from: lake_files` | kept, same wording: `relation "lake_files" is not queryable; query one of ...` |
| Refusals: 'x.parquet' | `FROM 'x.parquet'` | `from: 'x.parquet'` | kept: `expected a relation name, got "x.parquet"` |
| Refusals: $1 | parameter | no parameters | kept by construction: `unexpected character '$'` |
| Refusals: SELECT 1; SELECT 2 | two statements | one query per call | kept by construction: `SQL is not accepted; ...` (and `;` is not a token) |
| Refusals: empty, from/to missing, range > 31 days, from >= to | tool arguments | tool arguments | unchanged, except `sql is required` becomes `query is required` |
| Refusals: EVENTS is case-insensitive | `FROM EVENTS` | `from: EVENTS` | same (pinned) |
| Timeout | 14-way cross join | no joins | not expressible: no query here is quadratic. The bound is kept: Run returns `context.DeadlineExceeded` within 256 rows of the deadline (pinned: "cancelled"), and the tool keeps its 30 s message. The binding's test needs a slow RowSource |
| BusyIs429 | two cross joins hold both slots | a slow RowSource holds them | unchanged: the slots are the tool's |

## Proposed `query` entry (for the binding lane)

The entry lives in `internal/mcp/tools_ext.json`, not `tools.json` (which has
no `query`), so done-predicate 2's "tools.json unchanged" holds untouched.

```json
{
  "name": "query",
  "description": "Ask one read-only question over your tenant's sessions in mnemonic's query language: clauses, not SQL. Clauses, separated by | or newlines, each at most once: from: one relation (required); filter: a predicate; select: expressions or * for rows, or dimensions: and measures: for groups; sort: keys with asc or desc; limit: n. Example: from: events | filter: harness = 'codex' and tool_phase = 'request' | dimensions: tool_name | measures: count() as calls | sort: calls desc | limit: 10. Relations: events (one row per event: session_id, event_ts, harness, event_kind, actor_kind, tool_name, tool_call_id, tool_phase, model, input_tokens, output_tokens, latency_ms, content_types, text_content, author, ...; payload_json and token_usage_json are returned only when named), sessions (session_id, first_event_time, last_event_time, total_turns, total_events, tool_calls, tool_results, user_messages, assistant_messages, completed), turns (session_id, turn_seq, event_count, completed, terminal_uid), spans (session_id, span_id, parent_span_id, kind, name, start_time, end_time, status, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost), annotations (annotation_id, target_session_id, target_span_id, name, value_number, value_label, value_text, source, author, ts). Operators: = != < <= > >=, and, or, not, + - * /, in ('a', 'b'), like and ilike with % and _ (pattern at most 1000 bytes), is null, is not null, column match 'words' (full text: every word present), content_types includes 'image', map['key']. A 'string' compared with a time column is read as RFC 3339 or a date. Functions: lower, length, hour, day, week, month; in measures: count(), count(x), count_distinct, sum, avg, min, max, percentile(x, 0.95). Each relation holds only rows inside from..to (events by event_ts, spans by start_time, sessions overlapping the range, annotations by ts) and only sessions you may read. from and to are required RFC 3339 timestamps at most 31 days apart. At most limit rows come back (default 100, max 10000; truncated says more existed). A query runs at most 30 s and scans every part of the store, so keep filters selective. When all query slots are busy the server answers HTTP 429 with Retry-After.",
  "annotations": {"destructiveHint": false, "idempotentHint": true, "readOnlyHint": true},
  "inputSchema": {
    "type": "object",
    "additionalProperties": false,
    "required": ["query", "from", "to"],
    "properties": {
      "query": {"type": "string", "maxLength": 20000, "description": "One query, for example: from: sessions | filter: tool_calls > 50 | select: session_id, tool_calls | sort: tool_calls desc"},
      "from": {"type": "string", "description": "Start of the range, inclusive, as an RFC 3339 timestamp (for example 2026-10-01T00:00:00Z)."},
      "to": {"type": "string", "description": "End of the range, exclusive, as an RFC 3339 timestamp; at most 31 days after from."},
      "limit": {"type": "integer", "minimum": 1, "maximum": 10000, "default": 100, "description": "Maximum rows returned."}
    }
  }
}
```

## Binding notes

- The tool: decode args as today (`query` for `sql`), take a slot, `Parse`,
  `Check(q, ql.Relations)`, wrap the engine in a RowSource scoped to the
  caller and from..to, `Run` under the 30 s context with `Limits{Rows: limit,
  Held: 1_000_000}`, map `*ql.Error` to the argument error and
  `context.DeadlineExceeded` to the timeout message. `Plan.Columns()` is known
  before the run.
- The light row on lane/traces (`arena/trace/row.go:14-31`) carries fewer
  event columns than this table (no model, input_tokens, output_tokens,
  latency_ms, request_id, trace_id, turn_index, tool_error, ...). Either the
  light row grows, or those columns are marked `Heavy` here and read from the
  heavy record; the table is the one place to say which.
- `ingested_at`, `event_order`, `turn_seq` and `event_ordinal` are cache
  derivations; the engine has its own (`docs` order, turn_seq). If one has no
  engine source, delete it from the table rather than fake it.

## Not done here, by choice

- `final_filter:` (BTQL's having). No test or listed question needs it; it is
  one clause and one more pass over the group rows when one does.
- Joins and `in (from: ...)` subqueries. "Sessions with a low score" needs
  annotations joined to sessions; today that is two calls.
- `now()` and intervals: from..to already bounds every query.
