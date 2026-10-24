// Command e2e drives one mnemonic binary end to end through the Oct 22 done
// predicate (plan ~/.claude/plans/mnemonic-oss-scale-impl.md, checks a-f), as
// a black box: HTTP (push, OTLP/HTTP, MCP tools/call) plus the binary's own
// `export atif` CLI. It prints PASS/FAIL per check and writes a normalized
// transcript of every response.
//
//	go run ./arena/e2e -bin MNEMONIC -fixture RAWDIR -side old -out TRANSCRIPT
//
// Flags:
//
//	-bin      mnemonic binary to drive (server build: has serve and export)
//	-fixture  arena raw store whose raw/author=... tree supplies pushed sources
//	-side     old (SQL query tool, 59796bc) or engine (ql query tool); only the
//	          (d) request differs, every other request is identical
//	-out      transcript path (normalized; the mask list is printed in it)
//	-keep     keep the scratch data dir instead of removing it
//
// What a run does: scratch data dir, server.toml with three tokens (arena in
// tenant default, alice in acme with the annotate scope, bob in other); serve
// on a free loopback port; push 3 fixture sources over PUT /v1/raw as arena;
// (a) POST /v1/traces as alice with a gen_ai chat span and an execute_tool
// child, then get_trace; (b) annotate + get_trace, then a forced parser sweep
// (events/.parser-version rewritten with lowered otlp/annotation versions and
// the derived events parquet of alice's otlp and annotation sources removed,
// so serve re-parses them from raw) and get_trace again; (d) query refused
// without a time range, at most the row cap with one (per-side dialect);
// (e) bob's token sees nothing of alice's or arena's sessions through
// search_sessions, list_sessions, open, get_trace, query and file_attention;
// (f) delete_session hides a fixture session from search_sessions and open at
// once, and alice's otlp session from open and get_trace; then serve is
// stopped and (c) `export atif` for one claude-code and one codex fixture
// session is validated with internal/export.ValidateJSON, the same validator
// the binary links.
//
// Normalization masks volatile fields (ports, the scratch dir, wall-clock
// times, elapsed milliseconds, bm25 scores); the mask list heads the
// transcript. Two runs of the same binary must normalize identically.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/export"
)

// tokens and tenants the server config carries; alice needs annotate and
// admin (she annotates her own session, so admin is spare), arena and bob run
// on the default scopes (read, push).
const (
	tokArena = "e2e-arena-token-0123456789abcdef"
	tokAlice = "e2e-alice-token-0123456789abcdef"
	tokBob   = "e2e-bob-token-0123456789abcdef"

	authorArena = "arena"
	authorAlice = "alice"
	authorBob   = "bob"

	tenantAlice = "acme"
	tenantBob   = "other"

	machine = "m1"
	// wideStart/wideEnd bound every list_sessions and file_attention call: the
	// fixture's claude-code sessions ran in Oct 2026, its codex ones in Aug
	// 2026, and neither tool caps the span. The query tool caps it at 31 days,
	// so (d) and (e) query over dayStart..dayEnd, which holds the Oct
	// claude-code events.
	wideStart = "2026-08-01T00:00:00Z"
	wideEnd   = "2026-11-01T00:00:00Z"
	dayStart  = "2026-10-01T00:00:00Z"
	dayEnd    = "2026-11-01T00:00:00Z"

	otlpConv  = "e2e-conv-alice-1"
	otlpWord  = "e2ee2ealphaspan"
	otlpModel = "claude-sonnet-4-5"
)

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func main() {
	bin := flagString("bin", "", "mnemonic binary to drive")
	fixture := flagString("fixture", "", "arena raw store to push from")
	side := flagString("side", "old", "old or engine: the query dialect check (d) speaks")
	out := flagString("out", "", "transcript path")
	keep := flagBool("keep", false, "keep the scratch data dir")
	parse()

	if err := run(*bin, *fixture, *side, *out, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(2)
	}
}

type check struct {
	letter string
	name   string
	pass   bool
	notes  []string
}

func (c *check) failf(format string, args ...any) {
	c.pass = false
	c.notes = append(c.notes, fmt.Sprintf(format, args...))
}

func run(bin, fixture, side, out string, keep bool) error {
	if bin == "" || fixture == "" {
		return fmt.Errorf("-bin and -fixture are required")
	}
	if side != "old" && side != "engine" {
		return fmt.Errorf("-side must be old or engine")
	}
	tr := newTranscript()
	dataDir, err := os.MkdirTemp("", "e2e-mnemonic-")
	if err != nil {
		return err
	}
	defer func() {
		if keep {
			fmt.Fprintf(os.Stderr, "e2e: kept %s\n", dataDir)
			return
		}
		os.RemoveAll(dataDir)
	}()

	port, err := freePort()
	if err != nil {
		return err
	}
	tr.noteScratch(dataDir, fmt.Sprint(port))
	cfg := filepath.Join(dataDir, "server.toml")
	toml := fmt.Sprintf(`data_dir = %q
listen = "127.0.0.1:%d"

[[tokens]]
author = %q
sha256 = %q

[[tokens]]
author = %q
sha256 = %q
tenant = %q
scopes = ["read", "push", "annotate", "admin"]

[[tokens]]
author = %q
sha256 = %q
tenant = %q
`,
		dataDir, port, authorArena, sha256hex(tokArena),
		authorAlice, sha256hex(tokAlice), tenantAlice,
		authorBob, sha256hex(tokBob), tenantBob)
	if err := os.WriteFile(cfg, []byte(toml), 0o644); err != nil {
		return err
	}

	srv, err := startServer(bin, cfg, fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer srv.stop()
	if err := srv.waitReady(tr); err != nil {
		return err
	}

	env := &env{
		tr: tr, side: side, dataDir: dataDir,
		mcp:   &mcpClient{token: tokArena},
		alice: &mcpClient{token: tokAlice},
		bob:   &mcpClient{token: tokBob},
	}
	env.setBase(fmt.Sprintf("http://127.0.0.1:%d", port))

	if err := env.pushFixture(fixture); err != nil {
		return fmt.Errorf("push fixture: %w", err)
	}
	env.waitFixtureVisible()

	checks := []*check{
		env.checkA(),
		env.checkB(),
		env.checkD(),
		env.checkE(),
		env.checkF(),
	}
	env.pickExport()
	if err := srv.stop(); err != nil {
		tr.step("shutdown", "server stop: %v", err)
	} else {
		tr.step("shutdown", "serve stopped cleanly (SIGTERM)")
	}
	checks = append(checks, env.checkC(bin))

	pass := 0
	for _, c := range checks {
		state := "FAIL"
		if c.pass {
			state = "PASS"
			pass++
		}
		fmt.Fprintf(os.Stdout, "check (%s) %-64s %s\n", c.letter, c.name, state)
		for _, n := range c.notes {
			fmt.Fprintf(os.Stdout, "    %s\n", n)
		}
	}
	fmt.Fprintf(os.Stdout, "%d/6 checks PASS\n", pass)
	if out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, []byte(tr.render()), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "transcript: %s\n", out)
	}
	if pass != len(checks) {
		os.Exit(1)
	}
	return nil
}

// env is one run's state: the clients, the fixture facts the checks share,
// and the recorder.
type env struct {
	tr   *transcript
	side string

	dataDir string
	mcp     *mcpClient // arena, tenant default
	alice   *mcpClient // acme, annotate+admin
	bob     *mcpClient // other

	searchTerm      string         // words mined from the pushed fixture source
	searchHit       map[string]any // first fixture search hit
	pushed          []pushedSource
	deletedID       string // session (f) deleted, kept out of (c)
	otlpSession     string // alice's session:<...> id from (a)
	lastTraceResult map[string]any

	faCandidates []string          // file_path values mined from the pushed raw, most named first
	faPath       string            // the candidate file_attention answers for arena
	exportPick   map[string]string // one session id per harness for (c)
}

// ---- (a) OTLP chat span with an execute_tool child, get_trace shows it ----

func (e *env) checkA() *check {
	c := &check{letter: "a", name: "otlp chat+execute_tool lands; get_trace shows tree and tokens", pass: true}
	resp := e.alice.post("/v1/traces", "application/json", otlpBody())
	e.tr.step("a", "POST /v1/traces as alice: status %d, body %.120s", resp.status, resp.text)
	if resp.status != 200 {
		c.failf("/v1/traces status %d: %.200s", resp.status, resp.text)
		return c
	}
	if strings.Contains(resp.text, "partialSuccess") && !strings.Contains(resp.text, `"partialSuccess":{}`) {
		c.failf("spans rejected: %.300s", resp.text)
		return c
	}
	sess, ok := e.waitAliceSession(c)
	if !ok {
		return c
	}
	e.otlpSession = sess
	trace, terr := e.mcpCall(c, e.alice, "get_trace", map[string]any{"session_id": sess, "include_annotations": true})
	if trace == nil {
		c.failf("get_trace answered no data (%s: %s)", terr.code, terr.message)
		return c
	}
	tree, _ := trace["tree"].([]any)
	if len(tree) != 1 {
		c.failf("tree roots = %d, want 1", len(tree))
		return c
	}
	root, _ := tree[0].(map[string]any)
	child, _ := root["children"].([]any)
	if kind(root) != "chat" {
		c.failf("root kind = %q, want chat", kind(root))
	}
	if len(child) != 1 || kind(child[0]) != "execute_tool" {
		c.failf("root children = %d (first kind %q), want one execute_tool", len(child), firstKind(child))
	}
	tok, _ := root["tokens"].(map[string]any)
	if num(tok["input"]) != 1234 || num(tok["output"]) != 567 {
		c.failf("chat tokens = %v/%v, want 1234/567", num(tok["input"]), num(tok["output"]))
	}
	if len(child) == 1 {
		ct, _ := child[0].(map[string]any)["tokens"].(map[string]any)
		if num(ct["input"]) != 5 || num(ct["output"]) != 7 {
			c.failf("execute_tool tokens = %v/%v, want 5/7", num(ct["input"]), num(ct["output"]))
		}
	}
	totals, _ := trace["totals"].(map[string]any)
	tt, _ := totals["tokens"].(map[string]any)
	// totals are the roots' sums: the tool child's 5/7 hang beneath the chat
	// root and are not folded up, so the totals read 1234/567 over 2 spans
	if num(tt["input"]) != 1234 || num(tt["output"]) != 567 {
		c.failf("totals tokens = %v/%v, want 1234/567 (the roots' sum)", num(tt["input"]), num(tt["output"]))
	}
	if num(totals["spans"]) != 2 {
		c.failf("totals spans = %v, want 2", num(totals["spans"]))
	}
	if c.pass {
		e.tr.step("a", "get_trace: chat %q with one execute_tool child (5/7), chat tokens 1234/567, totals 1234/567 over 2 spans", str(root["name"]))
	}
	return c
}

// waitAliceSession polls list_sessions as alice until her otlp session is
// there, and returns its mcp-facing id.
func (e *env) waitAliceSession(c *check) (string, bool) {
	deadline := time.Now().Add(90 * time.Second)
	for {
		list, _ := e.mcpCall(c, e.alice, "list_sessions", map[string]any{
			"start_datetime": wideStart, "end_datetime": wideEnd, "harness": "otlp", "limit": 25,
		})
		for _, s := range arr(mapGet(list, "sessions")) {
			m, _ := s.(map[string]any)
			if id := str(m["id"]); id != "" {
				e.tr.step("a", "list_sessions(harness=otlp) as alice: %s harness=%s", id, str(m["harness"]))
				return id, true
			}
		}
		if time.Now().After(deadline) {
			c.failf("no otlp session in alice's list_sessions within 90s")
			return "", false
		}
		time.Sleep(1500 * time.Millisecond)
	}
}

// ---- (b) annotate survives a forced parser-version sweep ----

func (e *env) checkB() *check {
	c := &check{letter: "b", name: "annotation shows in get_trace and survives a forced parser sweep", pass: true}
	if e.otlpSession == "" {
		c.failf("no otlp session from (a)")
		return c
	}
	ann, terr := e.mcpCall(c, e.alice, "annotate", map[string]any{
		"session_id": e.otlpSession, "name": "correctness", "number": 0.75,
		"source": "scorer", "request_id": "e2e-ann-1",
	})
	if ann == nil {
		c.failf("annotate answered no data (%s: %s)", terr.code, terr.message)
		return c
	}
	annID := str(ann["annotation_id"])
	e.tr.step("b", "annotate: annotation_id=%s duplicate=%v", annID, ann["duplicate"])
	if !e.waitAnnotation(annID, 60*time.Second) {
		c.failf("annotation %s not visible in get_trace within 60s", annID)
		return c
	}

	e.forceSweep()
	if !e.waitAnnotation(annID, 120*time.Second) {
		c.failf("annotation %s gone after the forced sweep (120s)", annID)
		return c
	}
	trace := e.lastTraceResult
	if tree, _ := trace["tree"].([]any); len(tree) != 1 || kind(tree[0]) != "chat" {
		c.failf("tree changed after the sweep: %d roots", len(tree))
	}
	if c.pass {
		e.tr.step("b", "after sweep (marker rewritten, alice's otlp+annotation parquet removed): annotation %s still in get_trace, tree intact", annID)
	}
	return c
}

// waitAnnotation polls get_trace until annID shows, within max. The cache
// converges only after a cycle, so first sight lags the annotate call.
func (e *env) waitAnnotation(annID string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		trace, _ := e.mcpCall(e.scratchCheck(), e.alice, "get_trace", map[string]any{"session_id": e.otlpSession, "include_annotations": true})
		e.lastTraceResult = trace
		for _, a := range arr(mapGet(trace, "annotations")) {
			m, _ := a.(map[string]any)
			if str(m["id"]) == annID {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// forceSweep is the black-box parser-version sweep: the marker file the lake
// reads (events/.parser-version) records a lower version for the otlp and
// annotation parsers, and the derived events parquet of alice's otlp and
// annotation sources is removed, so the next cycle re-parses both from raw.
// A pushed source wakes the cycle — file edits alone would wait out the 5 min
// reconcile — and the sweep lap then revisits the unchanged sources and
// rewrites the marker, exactly the path a real parser bump takes.
func (e *env) forceSweep() {
	marker := filepath.Join(e.dataDir, "events", ".parser-version")
	if err := os.WriteFile(marker, []byte(`{"otlp":0,"annotation":0}`+"\n"), 0o644); err != nil {
		e.tr.step("b", "marker write failed: %v", err)
	}
	removed := 0
	root := filepath.Join(e.dataDir, "events", "author="+authorAlice)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".parquet") {
			if os.Remove(path) == nil {
				removed++
			}
		}
		return nil
	})
	wakeErr := "(no pushed source to re-send)"
	if len(e.pushed) > 0 {
		wakeErr = "(re-push failed)"
		if err := e.pushManifest(e.pushed[0].manifestPath, false); err == nil {
			wakeErr = "(woke with a re-push of " + filepath.Base(e.pushed[0].manifestPath) + ")"
		}
	}
	e.tr.step("b", "forced sweep: marker {otlp:0,annotation:0}, removed %d derived parquet under events/author=alice %s", removed, wakeErr)
}

// ---- (d) query: refused without a time range, at most the row cap with one ----

func (e *env) checkD() *check {
	c := &check{letter: "d", name: "query refused without a time range; at most the row cap with one", pass: true}
	if e.side == "engine" {
		e.queryEngine(c)
		return c
	}
	_, rte := e.mcpCall(c, e.mcp, "query", map[string]any{"sql": "SELECT session_id FROM events"})
	e.tr.step("d", "query without from/to (sql) as arena: refused=%v error=%q", rte.code != "", rte.message)
	if rte.code == "" {
		c.failf("query without a time range was not refused")
	}
	capped, _ := e.mcpCall(c, e.mcp, "query", map[string]any{
		"sql": "SELECT session_id FROM events", "from": dayStart, "to": dayEnd, "limit": 3,
	})
	rows := arr(mapGet(capped, "rows"))
	e.tr.step("d", "query with from/to, limit 3 (sql) as arena: row_count=%v truncated=%v rows=%d",
		mapGet(capped, "row_count"), mapGet(capped, "truncated"), len(rows))
	if n := num(mapGet(capped, "row_count")); n > 3 {
		c.failf("row_count %d above the cap 3", n)
	}
	if len(rows) == 0 {
		c.failf("no rows for a range that holds thousands of fixture events")
	}
	if mapGet(capped, "truncated") != true {
		c.failf("truncated is not true at the cap")
	}
	return c
}

// queryEngine speaks the ql dialect the engine's query tool takes; the
// semantic assertions are the same ones.
func (e *env) queryEngine(c *check) {
	_, rte := e.mcpCall(c, e.mcp, "query", map[string]any{"query": "from: events"})
	e.tr.step("d", "query without from/to (ql) as arena: refused=%v error=%q", rte.code != "", rte.message)
	if rte.code == "" {
		c.failf("query without a time range was not refused")
	}
	capped, _ := e.mcpCall(c, e.mcp, "query", map[string]any{
		"query": "from: events | select: session_id", "from": dayStart, "to": dayEnd, "limit": 3,
	})
	rows := arr(mapGet(capped, "rows"))
	e.tr.step("d", "query with from/to, limit 3 (ql) as arena: row_count=%v truncated=%v rows=%d",
		mapGet(capped, "row_count"), mapGet(capped, "truncated"), len(rows))
	if n := num(mapGet(capped, "row_count")); n > 3 {
		c.failf("row_count %d above the cap 3", n)
	}
	if len(rows) == 0 {
		c.failf("no rows for a range that holds thousands of fixture events")
	}
}

// ---- (e) tenant B sees nothing of tenant A through any tool ----

func (e *env) checkE() *check {
	c := &check{letter: "e", name: "tenant B's token sees none of tenant A's sessions via any tool", pass: true}
	bobList, _ := e.mcpCall(c, e.bob, "list_sessions", map[string]any{"start_datetime": wideStart, "end_datetime": wideEnd, "limit": 25})
	bobIDs := 0
	for _, s := range arr(mapGet(bobList, "sessions")) {
		if m, _ := s.(map[string]any); str(m["id"]) != "" {
			bobIDs++
		}
	}
	e.tr.step("e", "list_sessions 2026-10 as bob: %d sessions", bobIDs)
	if bobIDs != 0 {
		c.failf("bob lists %d sessions, want 0 (fixture sessions are arena's, alice's are hers)", bobIDs)
	}
	for _, q := range []string{otlpWord, e.searchTerm} {
		sr, _ := e.mcpCall(c, e.bob, "search_sessions", map[string]any{"query": q, "n_hits": 20})
		n := num(mapGet(sr, "result_count"))
		e.tr.step("e", "search_sessions %q as bob: result_count=%d", q, n)
		if n != 0 {
			c.failf("bob's search %q returned %d hits, want 0", q, n)
		}
	}
	if e.searchHit != nil {
		hitID := str(mapGet(e.searchHit, "id"))
		if hitID != "" {
			_, ote := e.mcpCall(c, e.bob, "open", map[string]any{"id": hitID})
			e.tr.step("e", "open %s as bob: refused=%v error=%q", hitID, ote.code != "", ote.message)
			if ote.code == "" {
				c.failf("bob opened arena's session %s", hitID)
			}
		}
	}
	if e.otlpSession != "" {
		_, tte := e.mcpCall(c, e.bob, "get_trace", map[string]any{"session_id": e.otlpSession})
		e.tr.step("e", "get_trace alice's session as bob: refused=%v error=%q", tte.code != "", tte.message)
		if tte.code == "" {
			c.failf("bob's get_trace read alice's session")
		}
	}
	// pick the mined path file_attention answers for arena (scope all, since
	// the fixture's paths sit outside the sessions' worktree roots), then ask
	// bob for the same path: an honest bob=0 needs arena>0 on the same call.
	for _, cand := range e.faCandidates {
		faArena, _ := e.mcpCall(c, e.mcp, "file_attention", map[string]any{
			"path": cand, "scope": "all", "start_datetime": wideStart, "end_datetime": wideEnd,
		})
		if n := len(arr(mapGet(faArena, "sessions"))); n > 0 {
			e.faPath = cand
			fa, _ := e.mcpCall(c, e.bob, "file_attention", map[string]any{
				"path": cand, "scope": "all", "start_datetime": wideStart, "end_datetime": wideEnd,
			})
			bobSessions := len(arr(mapGet(fa, "sessions")))
			e.tr.step("e", "file_attention %s (scope all): arena=%d sessions, bob=%d sessions", cand, n, bobSessions)
			if bobSessions != 0 {
				c.failf("bob's file_attention saw %d sessions, want 0", bobSessions)
			}
			break
		}
	}
	if e.faPath == "" {
		e.tr.step("e", "file_attention skipped: no file_path of the pushed sources answers for arena either")
	}
	q, _ := e.mcpCall(c, e.bob, "query", map[string]any{"sql": "SELECT session_id FROM events", "from": dayStart, "to": dayEnd, "limit": 5})
	bobRows := num(mapGet(q, "row_count"))
	qa, _ := e.mcpCall(c, e.mcp, "query", map[string]any{"sql": "SELECT session_id FROM events", "from": dayStart, "to": dayEnd, "limit": 5})
	arenaRows := num(mapGet(qa, "row_count"))
	e.tr.step("e", "query events as bob: %d rows; as arena: %d rows", bobRows, arenaRows)
	if bobRows != 0 {
		c.failf("bob's query returned %d rows, want 0", bobRows)
	}
	if arenaRows == 0 {
		c.failf("arena's query returned 0 rows; the bob=0 above proves nothing")
	}
	return c
}

// ---- (f) delete hides a session from search and open at once ----

func (e *env) checkF() *check {
	c := &check{letter: "f", name: "delete_session hides a session from search and open at once", pass: true}
	if e.searchHit == nil {
		c.failf("no fixture session handle from the smoke search")
		return c
	}
	target := str(mapGet(e.searchHit, "session_id"))
	if sess, _ := e.searchHit["session"].(map[string]any); target == "" && sess != nil {
		target = str(sess["id"])
	}
	if target == "" {
		c.failf("search hit carries no session id")
		return c
	}
	e.deletedID = target
	before, _ := e.mcpCall(c, e.mcp, "search_sessions", map[string]any{"query": e.searchTerm, "n_hits": 20})
	if n := num(mapGet(before, "result_count")); n < 1 {
		c.failf("pre-delete search found %d hits, want >= 1", n)
	}
	del, dte := e.mcpCall(c, e.mcp, "delete_session", map[string]any{"session_id": target})
	e.tr.step("f", "delete_session %s as arena: deleted=%v duplicate=%v error=%q",
		target, mapGet(del, "deleted"), mapGet(del, "duplicate"), dte.message)
	if mapGet(del, "deleted") != true {
		c.failf("delete_session did not report deleted=true (%s)", dte.message)
	}
	if !e.waitGone(target, 45*time.Second) {
		c.failf("deleted session %s still answers search or open after 45s", target)
	}
	// and alice's otlp session hides from open and get_trace the same way
	if e.otlpSession != "" {
		d2, _ := e.mcpCall(c, e.alice, "delete_session", map[string]any{"session_id": e.otlpSession})
		e.tr.step("f", "delete_session %s as alice: deleted=%v", e.otlpSession, mapGet(d2, "deleted"))
		deadline := time.Now().Add(45 * time.Second)
		for {
			_, o2 := e.mcpCall(c, e.alice, "open", map[string]any{"id": e.otlpSession})
			_, t2 := e.mcpCall(c, e.alice, "get_trace", map[string]any{"session_id": e.otlpSession})
			if o2.code != "" && t2.code != "" {
				e.tr.step("f", "alice's otlp session now refused by open and get_trace")
				break
			}
			if time.Now().After(deadline) {
				c.failf("alice's deleted otlp session still answers open or get_trace after 45s")
				break
			}
			time.Sleep(2 * time.Second)
		}
	}
	return c
}

// waitGone polls until target is out of the fixture search and open refuses
// it, within max: the delete lands as a control record the cache converges
// a cycle later.
func (e *env) waitGone(target string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		after, _ := e.mcpCall(e.scratchCheck(), e.mcp, "search_sessions", map[string]any{"query": e.searchTerm, "n_hits": 20})
		still := false
		for _, h := range arr(mapGet(after, "results")) {
			m, _ := h.(map[string]any)
			s, _ := m["session"].(map[string]any)
			if str(s["id"]) == target {
				still = true
			}
		}
		_, ote := e.mcpCall(e.scratchCheck(), e.mcp, "open", map[string]any{"id": target})
		if !still && ote.code != "" {
			e.tr.step("f", "after delete: search hits=%d, open refused (%s)", num(mapGet(after, "result_count")), ote.code)
			return true
		}
		if time.Now().After(deadline) {
			e.tr.step("f", "after delete (deadline): search hits=%d, open refused=%v", num(mapGet(after, "result_count")), ote.code != "")
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// pickExport records, while the server is still up, one exportable session id
// per harness (the first in list order that (f) did not delete).
func (e *env) pickExport() {
	c := &check{letter: "c", pass: true}
	list, _ := e.mcpCall(c, e.mcp, "list_sessions", map[string]any{"start_datetime": wideStart, "end_datetime": wideEnd, "limit": 25})
	e.exportPick = map[string]string{}
	for _, s := range arr(mapGet(list, "sessions")) {
		m, _ := s.(map[string]any)
		sess, _ := m["session"].(map[string]any)
		id, h := str(m["id"]), str(sess["harness"])
		if id == "" || id == e.deletedID || e.exportPick[h] != "" {
			continue
		}
		e.exportPick[h] = id
	}
	e.tr.step("shutdown", "sessions picked for (c), one per harness: %v", e.exportPick)
}

// ---- (c) export atif passes the ATIF validator, one session per harness ----

func (e *env) checkC(bin string) *check {
	c := &check{letter: "c", name: "export atif for one session per harness passes the ATIF validator", pass: true}
	pick := e.exportPick
	harnesses := make([]string, 0, len(pick))
	for h := range pick {
		harnesses = append(harnesses, h)
	}
	sort.Strings(harnesses)
	for _, h := range harnesses {
		out := filepath.Join(e.dataDir, "atif-"+strings.ReplaceAll(h, "/", "_")+".json")
		cmd := exec.Command(bin, "export", "atif", "--data-dir", e.dataDir, "--session", pick[h], "--out", out)
		b, err := cmd.CombinedOutput()
		if err != nil {
			c.failf("export atif (%s): %v: %.200s", h, err, string(b))
			continue
		}
		doc, err := os.ReadFile(out)
		if err != nil {
			c.failf("read export output (%s): %v", h, err)
			continue
		}
		errs := export.ValidateJSON(doc)
		e.tr.step("c", "export atif harness=%s session=%s bytes=%d validator_errors=%d excerpt=%.240s",
			h, pick[h], len(doc), len(errs), string(doc))
		for _, v := range errs {
			c.failf("ATIF validation (%s): %s", h, v)
		}
	}
	if len(harnesses) == 0 {
		c.failf("no exportable session per harness (deleted=%s)", e.deletedID)
	}
	return c
}

// scratchCheck absorbs the trace poll records so the (b) narrative stays
// readable; a poll that errors marks nothing.
func (e *env) scratchCheck() *check { return &check{letter: "b", pass: true} }
