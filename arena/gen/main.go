// Command gen draws arena calls from the oracle's index and writes calls.jsonl
// and, through `oracle answer`, expected.jsonl into -out.
//
//	gen -root ORACLEROOT -oracle BIN -seed N -out DIR
package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

type call struct {
	ID   int            `json:"id"`
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type result struct {
	ID    int             `json:"id"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

var eventTypes = []string{"user_input", "assistant_response", "reasoning", "tool_call", "tool_response", "compaction", "system", "runtime"}

func main() {
	root := flag.String("root", "", "oracle data root")
	oracle := flag.String("oracle", "", "oracle binary")
	seed := flag.Int64("seed", 1, "rng seed")
	out := flag.String("out", "", "dir for calls.jsonl and expected.jsonl")
	flag.Parse()
	if err := run(*root, *oracle, *seed, *out); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

type corpus struct {
	strata   [][]string
	paths    []string
	from, to time.Time
}

func load(root string) (corpus, error) {
	var c corpus
	db, err := sql.Open("duckdb", filepath.Join(root, "cache", "index.duckdb")+"?access_mode=read_only")
	if err != nil {
		return c, err
	}
	defer db.Close()
	bounds := []struct{ lo, hi int }{{1, 1}, {2, 9}, {10, 99}, {100, 999}, {1000, 1 << 30}}
	for _, b := range bounds {
		terms, err := strings(db, `SELECT term FROM (SELECT term, count(DISTINCT doc) AS df FROM postings
			WHERE regexp_full_match(term, '[a-z][a-z0-9]{2,15}') GROUP BY term)
			WHERE df BETWEEN ? AND ? ORDER BY term`, b.lo, b.hi)
		if err != nil {
			return c, err
		}
		c.strata = append(c.strata, terms)
	}
	c.paths, err = strings(db, `SELECT p FROM (SELECT tool_io_repo_rel_path AS p, count(*) AS n FROM events
		WHERE coalesce(tool_io_repo_rel_path, '') <> '' AND NOT starts_with(tool_io_repo_rel_path, '/')
		GROUP BY 1) ORDER BY n DESC, p`)
	if err != nil {
		return c, err
	}
	err = db.QueryRow("SELECT min(first_event_time), max(last_event_time) FROM sessions").Scan(&c.from, &c.to)
	return c, err
}

func strings(db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func run(root, oracle string, seed int64, out string) error {
	c, err := load(root)
	if err != nil {
		return err
	}
	for i, s := range c.strata {
		if len(s) == 0 {
			return fmt.Errorf("stratum %d is empty", i)
		}
	}
	if len(c.paths) == 0 {
		// Repo roots are probed on the parsing machine's disk, so a corpus parsed away from its repos has none.
		c.paths = []string{"AGENTS.md", "README.md", "go.mod", "package.json", "main.go"}
		fmt.Fprintln(os.Stderr, "gen: no repo-relative tool paths (corpus parsed away from its repos); file_attention draws common names and answers empty")
	}
	r := rand.New(rand.NewSource(seed))
	var calls []call
	add := func(tool string, args map[string]any) {
		calls = append(calls, call{ID: len(calls) + 1, Tool: tool, Args: args})
	}
	for range 40 {
		add("search_sessions", searchArgs(r, c))
	}
	for range 12 {
		add("file_attention", attentionArgs(r, c))
	}
	for range 4 {
		add("list_sessions", listArgs(r, c))
	}
	add("get_ingest_status", map[string]any{})

	res, err := answer(oracle, root, calls)
	if err != nil {
		return err
	}
	var hits []hit
	for i, cl := range calls {
		if cl.Tool == "search_sessions" && res[i].Error == "" {
			hits = append(hits, hitsOf(res[i].Data)...)
		}
	}
	if len(hits) < 4 {
		return fmt.Errorf("only %d search hits to open", len(hits))
	}
	r.Shuffle(len(hits), func(i, j int) { hits[i], hits[j] = hits[j], hits[i] })
	add("open", map[string]any{"id": hits[0].Event})
	add("open", map[string]any{"id": hits[1].Turn, "limit": 5})
	add("open", map[string]any{"id": hits[2].Session})
	add("open", map[string]any{"id": hits[3].Session, "limit": 3})
	pager := len(calls)
	add("open", map[string]any{"id": hits[0].Session, "limit": 2})

	res, err = answer(oracle, root, calls)
	if err != nil {
		return err
	}
	var paged struct {
		NextCursor string `json:"next_cursor"`
	}
	json.Unmarshal(res[pager].Data, &paged)
	if paged.NextCursor == "" {
		return fmt.Errorf("open session limit 2 gave no next_cursor")
	}
	add("open", map[string]any{"cursor": paged.NextCursor})

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, cl := range calls {
		b, _ := json.Marshal(cl)
		buf.Write(append(b, '\n'))
	}
	if err := os.WriteFile(filepath.Join(out, "calls.jsonl"), buf.Bytes(), 0o644); err != nil {
		return err
	}
	exp, err := oracleRun(oracle, root, buf.Bytes())
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, cl := range calls {
		counts[cl.Tool]++
	}
	fmt.Fprintf(os.Stderr, "gen seed %d: %d calls %v\n", seed, len(calls), counts)
	return os.WriteFile(filepath.Join(out, "expected.jsonl"), exp, 0o644)
}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.Intn(len(xs))] }

func searchArgs(r *rand.Rand, c corpus) map[string]any {
	n := 1 + r.Intn(4)
	q := ""
	for i := range n {
		if i > 0 {
			q += " "
		}
		q += pick(r, pick(r, c.strata))
	}
	a := map[string]any{"query": q}
	switch r.Intn(4) {
	case 0:
		a["harness"] = "claude-code"
	case 1:
		a["harness"] = "codex"
	}
	if r.Intn(3) == 0 {
		perm := r.Perm(len(eventTypes))[:1+r.Intn(3)]
		sort.Ints(perm)
		var ts []string
		for _, p := range perm {
			ts = append(ts, eventTypes[p])
		}
		a["event_types"] = ts
	}
	if k := []int{0, 10, 20, 50}[r.Intn(4)]; k > 0 {
		a["n_hits"] = k
	}
	return a
}

func attentionArgs(r *rand.Rand, c corpus) map[string]any {
	var p string
	switch r.Intn(3) {
	case 0:
		p = c.paths[r.Intn(min(10, len(c.paths)))]
	case 1:
		p = c.paths[r.Intn(len(c.paths))]
	default:
		p = c.paths[len(c.paths)/2+r.Intn(len(c.paths)-len(c.paths)/2)]
	}
	a := map[string]any{"path": p}
	if s := []string{"", "project", "all", "all", "all"}[r.Intn(5)]; s != "" {
		a["scope"] = s
	}
	if g := []string{"", "sessions", "events"}[r.Intn(3)]; g != "" {
		a["granularity"] = g
	}
	if r.Intn(3) == 0 {
		a["mutations_only"] = r.Intn(2) == 0
	}
	if l := []int{0, 3, 10, 25}[r.Intn(4)]; l > 0 {
		a["limit"] = l
	}
	return a
}

func listArgs(r *rand.Rand, c corpus) map[string]any {
	span := c.to.Sub(c.from)
	start := c.from.Add(time.Duration(r.Int63n(int64(span/2) + 1))).Truncate(time.Hour)
	end := start.Add(span / time.Duration(1+r.Intn(3))).Truncate(time.Hour).Add(time.Hour)
	a := map[string]any{
		"start_datetime": start.UTC().Format(time.RFC3339),
		"end_datetime":   end.UTC().Format(time.RFC3339),
	}
	if l := []int{0, 5, 25}[r.Intn(3)]; l > 0 {
		a["limit"] = l
	}
	if r.Intn(2) == 0 {
		a["sort"] = pick(r, []string{"asc", "desc"})
	}
	if r.Intn(3) == 0 {
		a["harness"] = pick(r, []string{"claude-code", "codex"})
	}
	return a
}

type hit struct{ Event, Turn, Session string }

func hitsOf(data json.RawMessage) []hit {
	var d struct {
		Results []struct {
			Open struct {
				EventID   string `json:"event_id"`
				TurnID    string `json:"turn_id"`
				SessionID string `json:"session_id"`
			} `json:"open"`
		} `json:"results"`
	}
	json.Unmarshal(data, &d)
	var hs []hit
	for _, x := range d.Results {
		if x.Open.EventID != "" && x.Open.TurnID != "" && x.Open.SessionID != "" {
			hs = append(hs, hit{x.Open.EventID, x.Open.TurnID, x.Open.SessionID})
		}
	}
	return hs
}

func answer(oracle, root string, calls []call) ([]result, error) {
	var in bytes.Buffer
	for _, cl := range calls {
		b, _ := json.Marshal(cl)
		in.Write(append(b, '\n'))
	}
	out, err := oracleRun(oracle, root, in.Bytes())
	if err != nil {
		return nil, err
	}
	var res []result
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var x result
		if err := json.Unmarshal(sc.Bytes(), &x); err != nil {
			return nil, err
		}
		res = append(res, x)
	}
	if len(res) != len(calls) {
		return nil, fmt.Errorf("oracle answered %d of %d calls", len(res), len(calls))
	}
	return res, nil
}

func oracleRun(oracle, root string, in []byte) ([]byte, error) {
	cmd := exec.Command(oracle, "answer", "-root", root)
	cmd.Stdin = bytes.NewReader(in)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}
