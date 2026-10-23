package main

// The driving side: one server process, an MCP-over-HTTP client, the push
// path, and the transcript with its normalizer.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
)

var flags = flag.NewFlagSet("e2e", flag.ExitOnError)

func flagString(name, def, usage string) *string { return flags.String(name, def, usage) }
func flagBool(name string, def bool, usage string) *bool {
	return flags.Bool(name, def, usage)
}
func parse() { flags.Parse(os.Args[1:]) }

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ---- server process ----

type serverProc struct {
	cmd  *exec.Cmd
	addr string
	log  *bytes.Buffer
}

func startServer(bin, cfg, addr string) (*serverProc, error) {
	cmd := exec.Command(bin, "serve", "--config", cfg)
	cmd.Stdout = io.Discard
	log := &bytes.Buffer{}
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &serverProc{cmd: cmd, addr: addr, log: log}, nil
}

// stop SIGTERMs the server and waits; a server still alive after 12 s is
// killed. Stopping twice is fine.
func (s *serverProc) stop() error {
	if s == nil || s.cmd.Process == nil {
		return nil
	}
	s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
		s.cmd.Process = nil
		return nil // a non-zero exit after SIGTERM is ordinary
	case <-time.After(12 * time.Second):
		s.cmd.Process.Kill()
		<-done
		s.cmd.Process = nil
		return fmt.Errorf("server did not exit within 12s of SIGTERM; killed")
	}
}

// waitReady polls until the server accepts TCP; the listener comes up only
// after the cache is open, so an accepted connection means serving.
func (s *serverProc) waitReady(tr *transcript) error {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", s.addr)
		if err == nil {
			conn.Close()
			tr.step("boot", "serve is accepting connections on %s", s.addr)
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("server not accepting after 45s; stderr tail:\n%.2000s", s.logTail())
}

func (s *serverProc) logTail() string {
	t := s.log.String()
	if len(t) > 2000 {
		t = t[len(t)-2000:]
	}
	return t
}

// ---- MCP over HTTP ----

type mcpClient struct {
	base   string
	token  string
	nextID int
}

type httpResult struct {
	status int
	header http.Header
	body   []byte
	text   string
}

func (c *mcpClient) do(method, path, contentType string, body []byte) httpResult {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return httpResult{status: 0, body: []byte(err.Error())}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if path == "/mcp" {
		req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return httpResult{status: 0, body: []byte(err.Error())}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return httpResult{status: resp.StatusCode, header: resp.Header, body: b, text: string(b)}
}

func (c *mcpClient) post(path, contentType string, body []byte) httpResult {
	return c.do(http.MethodPost, path, contentType, body)
}

// call issues one tools/call and returns result.structuredContent (the tool
// envelope: its data holds the payload, its error the refusal). A transport
// or JSON failure returns nil with the raw answer as the message.
func (c *mcpClient) call(tool string, args map[string]any) (env map[string]any, isErr bool, msg string) {
	c.nextID++
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	}
	b, _ := json.Marshal(req)
	resp := c.post("/mcp", "application/json", b)
	var outer struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Structured map[string]any `json:"structuredContent"`
			IsError    bool           `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int64  `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.body, &outer); err != nil {
		return nil, true, fmt.Sprintf("status %d, undecodable: %v: %.300s", resp.status, err, resp.text)
	}
	if outer.Error != nil {
		return nil, true, fmt.Sprintf("rpc error %d: %s", outer.Error.Code, outer.Error.Message)
	}
	if outer.Result == nil {
		return nil, true, fmt.Sprintf("status %d, no result: %.300s", resp.status, resp.text)
	}
	env = outer.Result.Structured
	if outer.Result.IsError {
		terr, _ := env["error"].(map[string]any)
		return env, true, str(terr["message"])
	}
	return env, false, ""
}

// ---- transcript ----

type mask struct {
	name string
	re   *regexp.Regexp
	repl string
}

type entry struct {
	step string
	line string
}

type transcript struct {
	entries []entry
	masks   []mask
}

func newTranscript() *transcript {
	re := func(s string) *regexp.Regexp { return regexp.MustCompile(s) }
	return &transcript{masks: []mask{
		{"rfc3339 timestamps", re(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})`), "<TS>"},
		{"unix millis", re(`\b1[0-9]{12}\b`), "<MS>"},
		{"unix nanos", re(`\b1[0-9]{17,18}\b`), "<NS>"},
		{"elapsed_ms", re(`"elapsed_ms":\d+`), `"elapsed_ms":<ELAPSED>`},
		{"bm25 scores", re(`"score":(-?[0-9.eE+]+)`), `"score":<SCORE>`},
	}}
}

func (t *transcript) step(step, format string, args ...any) {
	t.entries = append(t.entries, entry{step, fmt.Sprintf(format, args...)})
}

// noteScratch teaches the transcript this run's volatile dir and port, so the
// normalizer can mask them wherever they appear.
func (t *transcript) noteScratch(dir, port string) {
	t.masks = append(t.masks,
		mask{"scratch data dir", regexp.MustCompile(regexp.QuoteMeta(dir)), "<DATA>"},
		mask{"loopback host:port", regexp.MustCompile(`127\.0\.0\.1:` + port), "<HOST>"},
	)
}

func (t *transcript) norm(s string) string {
	for _, m := range t.masks {
		s = m.re.ReplaceAllString(s, m.repl)
	}
	return s
}

func (t *transcript) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "mnemonic e2e transcript (normalized)\n")
	fmt.Fprintf(&b, "mask list (applied in this order at render time):\n")
	for _, m := range t.masks {
		fmt.Fprintf(&b, "  %-22s /%s/ -> %s\n", m.name, m.re, m.repl)
	}
	fmt.Fprintf(&b, "tool responses are recorded to their first %d bytes; push and export bodies to a fixed excerpt\n\n", mcpRecordCap)
	step := ""
	for _, e := range t.entries {
		if e.step != step {
			step = e.step
			fmt.Fprintf(&b, "== %s ==\n", step)
		}
		fmt.Fprintf(&b, "%s\n", t.norm(e.line))
	}
	return b.String()
}

// ---- fixture push ----

type pushedSource struct {
	manifestPath string
	manifest     map[string]any
	rawBytes     int
}

// pushFixture picks three fixture sources (the first two claude-code and the
// second codex manifest, by src_id order), decompresses their raw, and PUTs
// each to /v1/raw under the arena token with the manifest's own head_hash,
// source_name and path.
func (e *env) pushFixture(fixtureRoot string) error {
	wants := []struct {
		harness string
		index   int
	}{{"claude-code", 0}, {"claude-code", 1}, {"codex", 1}}
	zr, err := zstd.NewReader(nil)
	if err != nil {
		return err
	}
	defer zr.Close()
	var terms []string
	for _, w := range wants {
		dir := filepath.Join(fixtureRoot, "author="+authorArena, "machine="+machine, "harness="+w.harness)
		mans, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			return err
		}
		sort.Strings(mans)
		band := mans
		if len(mans) > 4 {
			band = mans[:4]
		}
		if len(band) <= w.index {
			return fmt.Errorf("only %d sources under %s", len(band), dir)
		}
		man := band[w.index]
		b, err := os.ReadFile(man)
		if err != nil {
			return err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s: %w", man, err)
		}
		zst := strings.TrimSuffix(man, ".json") + ".jsonl.zst"
		comp, err := os.ReadFile(zst)
		if err != nil {
			return err
		}
		raw, err := zr.DecodeAll(comp, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", zst, err)
		}
		q := fmt.Sprintf("/v1/raw/%s/%s/%s?offset=0&head_hash=%s&source_name=%s&path=%s&raw_end=%d",
			url.QueryEscape(machine), url.QueryEscape(str(m["harness"])), url.QueryEscape(str(m["src_id"])),
			url.QueryEscape(str(m["head_hash"])), url.QueryEscape(str(m["source_name"])),
			url.QueryEscape(str(m["path"])), int64(num(m["raw_end"])))
		resp := e.mcp.post(q, "application/octet-stream", raw)
		e.pushed = append(e.pushed, pushedSource{manifestPath: man, manifest: m, rawBytes: len(raw)})
		e.tr.step("push", "PUT /v1/raw/%s/%s (src_id %s): status %d, %d raw bytes",
			machine, str(m["harness"]), str(m["src_id"]), resp.status, len(raw))
		if resp.status != 200 {
			return fmt.Errorf("PUT %s: status %d: %s", q, resp.status, resp.text)
		}
		if w := searchWords(raw); w != "" {
			terms = append(terms, w)
		}
	}
	if len(terms) == 0 {
		return fmt.Errorf("no searchable words mined from the pushed sources")
	}
	e.searchTerm = terms[0]
	e.tr.step("push", "search term for the smoke probe: %q", e.searchTerm)
	return nil
}

// searchWords takes the first six words of the first line's content field, so
// the smoke search runs on real fixture user input.
func searchWords(raw []byte) string {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		content := str(m["content"])
		if content == "" {
			continue
		}
		words := strings.Fields(content)
		if len(words) > 6 {
			words = words[:6]
		}
		var keep []string
		for _, w := range words {
			w = strings.Trim(w, "\"',.;:!?()[]")
			if w != "" && !strings.ContainsAny(w, "{}[]|<>") {
				keep = append(keep, w)
			}
		}
		if len(keep) >= 3 {
			return strings.Join(keep, " ")
		}
	}
	return ""
}

// waitFixtureVisible polls search_sessions as arena until the fixture words
// answer, records the first hit, and mines a touched file path for (e).
func (e *env) waitFixtureVisible() {
	deadline := time.Now().Add(120 * time.Second)
	for {
		env, isErr, msg := e.mcp.call("search_sessions", map[string]any{"query": e.searchTerm, "n_hits": 20})
		if !isErr {
			if n := num(mapGet(env, "result_count")); n > 0 {
				hits := arr(mapGet(env, "results"))
				e.searchHit, _ = hits[0].(map[string]any)
				sess, _ := e.searchHit["session"].(map[string]any)
				e.tr.step("boot", "fixture visible: search %q result_count=%d first session=%s",
					e.searchTerm, n, str(mapGet(sess, "id")))
				e.mineFilePath()
				return
			}
		} else {
			e.tr.step("boot", "search_sessions not ready: %s", msg)
		}
		if time.Now().After(deadline) {
			e.tr.step("boot", "fixture never became searchable within 120s")
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// mineFilePath pulls a file_path out of the first pushed source's raw, for
// the file_attention half of (e); empty when the source names none.
func (e *env) mineFilePath() {
	zr, err := zstd.NewReader(nil)
	if err != nil {
		return
	}
	defer zr.Close()
	zst := strings.TrimSuffix(e.pushed[0].manifestPath, ".json") + ".jsonl.zst"
	comp, err := os.ReadFile(zst)
	if err != nil {
		return
	}
	raw, err := zr.DecodeAll(comp, nil)
	if err != nil {
		return
	}
	if m := regexp.MustCompile(`"file_path"\s*:\s*"([^"]+)"`).FindSubmatch(raw); m != nil {
		e.faPath = string(m[1])
	}
}

// ---- shared helpers ----

const mcpRecordCap = 1200

func (e *env) setBase(base string) {
	for _, c := range []*mcpClient{e.mcp, e.alice, e.bob} {
		c.base = base
	}
}

// mcpCall wraps call for the checks: it records the exchange and returns the
// envelope; a failure marks the check and is recorded.
func (e *env) mcpCall(c *check, cl *mcpClient, tool string, args map[string]any) map[string]any {
	envd, isErr, msg := cl.call(tool, args)
	ab := abbrevArgs(args)
	if isErr {
		e.tr.step(c.letter, "tools/call %s %s -> error: %s", tool, ab, msg)
		if envd == nil {
			c.failf("%s failed: %s", tool, msg)
		}
		return envd
	}
	e.tr.step(c.letter, "tools/call %s %s -> %s", tool, ab, jsonOne(envd, mcpRecordCap))
	return envd
}

func abbrevArgs(args map[string]any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	s := string(b)
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

type toolErr struct{ code, message string }

// toolError reads an error envelope out of a call result, "" when none.
func toolError(data map[string]any) toolErr {
	if data == nil {
		return toolErr{}
	}
	terr, _ := data["error"].(map[string]any)
	return toolErr{code: str(terr["code"]), message: str(terr["message"])}
}

// otlpBody is the (a) request: one resourceSpans holding a gen_ai chat span
// and its execute_tool child, both with the conversation id, the chat with
// 1234/567 tokens, the tool with 5/7. Times are fixed, so runs compare.
func otlpBody() []byte {
	const traceID = "11223344556677889900112233445566"
	const chatSpan = "aabbccddeeff0001"
	const toolSpan = "aabbccddeeff0002"
	var start uint64 = 1_792_000_000_000_000_000 // fixed, in ns
	end := start + 3_000_000_000
	span := func(id, parent, name, op string, extra map[string]any, in, out uint64, st, en uint64) map[string]any {
		attrs := map[string]any{
			"gen_ai.conversation.id": otlpConv,
			"gen_ai.operation.name":  op,
		}
		for k, v := range extra {
			attrs[k] = v
		}
		if in > 0 {
			attrs["gen_ai.usage.input_tokens"] = int(in)
			attrs["gen_ai.usage.output_tokens"] = int(out)
		}
		m := map[string]any{
			"traceId":           traceID,
			"spanId":            id,
			"name":              name,
			"kind":              3,
			"startTimeUnixNano": st,
			"endTimeUnixNano":   en,
			"attributes":        attrs,
			"status":            map[string]any{"code": 1},
		}
		if parent != "" {
			m["parentSpanId"] = parent
		}
		return m
	}
	body := map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []any{
				map[string]any{"key": "service.name", "value": map[string]any{"stringValue": "e2e-otlp"}},
			}},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "e2e", "version": "1"},
				"spans": []any{
					span(chatSpan, "", otlpWord, "chat",
						map[string]any{"gen_ai.request.model": otlpModel}, 1234, 567, start, end),
					span(toolSpan, chatSpan, "read_file", "execute_tool",
						map[string]any{"gen_ai.tool.name": "read_file", "gen_ai.tool.call.id": "call_e2e_1"}, 5, 7, end, end+1_000_000_000),
				},
			}},
		}},
	}
	b, _ := json.Marshal(body)
	return b
}

func jsonOne(v any, cap int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	s := string(b)
	if len(s) > cap {
		s = s[:cap] + "..."
	}
	return s
}

func mapGet(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return -1
}

func kind(span any) string {
	m, _ := span.(map[string]any)
	return str(mapGet(m, "kind"))
}

func firstKind(children []any) string {
	if len(children) == 0 {
		return ""
	}
	return kind(children[0])
}
