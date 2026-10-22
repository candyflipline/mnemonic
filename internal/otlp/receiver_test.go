package otlp_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	colltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/cache"
	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/otlp"
	"github.com/candyflipline/mnemonic/internal/query"
	"github.com/candyflipline/mnemonic/internal/server"
	"github.com/candyflipline/mnemonic/internal/store"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const tracesJSON = `{"resourceSpans":[{
  "resource":{"attributes":[{"key":"service.name","value":{"stringValue":"Agent App"}}]},
  "scopeSpans":[{"scope":{"name":"demo","version":"1"},"spans":[
    {"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"invoke_agent helper","kind":1,
     "startTimeUnixNano":"1760000000000000000","endTimeUnixNano":"1760000009000000000","status":{"code":1},
     "attributes":[
       {"key":"gen_ai.operation.name","value":{"stringValue":"invoke_agent"}},
       {"key":"gen_ai.conversation.id","value":{"stringValue":"conv-1"}},
       {"key":"gen_ai.agent.name","value":{"stringValue":"helper"}}]},
    {"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b175","parentSpanId":"eee19b7ec3c1b174","name":"chat gpt-5","kind":3,
     "startTimeUnixNano":"1760000001000000000","endTimeUnixNano":"1760000004000000000","status":{"code":1},
     "attributes":[
       {"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},
       {"key":"gen_ai.request.model","value":{"stringValue":"gpt-5"}},
       {"key":"gen_ai.usage.input_tokens","value":{"intValue":"120"}},
       {"key":"gen_ai.usage.output_tokens","value":{"intValue":30}},
       {"key":"gen_ai.usage.cache_read.input_tokens","value":{"intValue":"20"}},
       {"key":"gen_ai.usage.cost","value":{"doubleValue":0.0123}},
       {"key":"gen_ai.input.messages","value":{"stringValue":"[{\"role\":\"user\",\"content\":\"hunter2\"}]"}},
       {"key":"gen_ai.output.messages","value":{"stringValue":"[{\"role\":\"assistant\",\"content\":\"ok\"}]"}}]},
    {"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b176","parentSpanId":"eee19b7ec3c1b175","name":"retrieve","kind":1,
     "startTimeUnixNano":"1760000004000000000","endTimeUnixNano":"1760000005000000000"},
    {"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b177","parentSpanId":"eee19b7ec3c1b176","name":"execute_tool bash","kind":1,
     "startTimeUnixNano":"1760000005000000000","endTimeUnixNano":"1760000006500000000","status":{"code":2},
     "attributes":[
       {"key":"gen_ai.operation.name","value":{"stringValue":"execute_tool"}},
       {"key":"gen_ai.tool.name","value":{"stringValue":"bash"}},
       {"key":"gen_ai.tool.call.id","value":{"stringValue":"call_1"}},
       {"key":"gen_ai.tool.call.arguments","value":{"stringValue":"rm -rf /"}}]},
    {"traceId":"5b8efff798038103d269b633813fc60c","spanId":"","name":"no id"}
  ]}]}]}`

const rawNoContent = `{"signal":"span","session_id":"conv-1","author":"chris","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b174","name":"invoke_agent helper","span_kind":1,"start_unix_nano":"1760000000000000000","end_unix_nano":"1760000009000000000","status_code":1,"attributes":{"gen_ai.agent.name":"helper","gen_ai.conversation.id":"conv-1","gen_ai.operation.name":"invoke_agent"},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":false}
{"signal":"span","session_id":"conv-1","author":"chris","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b175","parent_span_id":"eee19b7ec3c1b174","name":"chat gpt-5","span_kind":3,"start_unix_nano":"1760000001000000000","end_unix_nano":"1760000004000000000","status_code":1,"attributes":{"gen_ai.operation.name":"chat","gen_ai.request.model":"gpt-5","gen_ai.usage.cache_read.input_tokens":20,"gen_ai.usage.cost":0.0123,"gen_ai.usage.input_tokens":120,"gen_ai.usage.output_tokens":30},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":false}
{"signal":"span","session_id":"conv-1","author":"chris","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b176","parent_span_id":"eee19b7ec3c1b175","name":"retrieve","span_kind":1,"start_unix_nano":"1760000004000000000","end_unix_nano":"1760000005000000000","resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":false}
{"signal":"span","session_id":"conv-1","author":"chris","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b177","parent_span_id":"eee19b7ec3c1b176","name":"execute_tool bash","span_kind":1,"start_unix_nano":"1760000005000000000","end_unix_nano":"1760000006500000000","status_code":2,"attributes":{"gen_ai.operation.name":"execute_tool","gen_ai.tool.call.id":"call_1","gen_ai.tool.name":"bash"},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":false}
`

const rawWithContent = `{"signal":"span","session_id":"conv-1","author":"chris","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b174","name":"invoke_agent helper","span_kind":1,"start_unix_nano":"1760000000000000000","end_unix_nano":"1760000009000000000","status_code":1,"attributes":{"gen_ai.agent.name":"helper","gen_ai.conversation.id":"conv-1","gen_ai.operation.name":"invoke_agent"},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":true}
{"signal":"span","session_id":"conv-1","author":"chris","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b175","parent_span_id":"eee19b7ec3c1b174","name":"chat gpt-5","span_kind":3,"start_unix_nano":"1760000001000000000","end_unix_nano":"1760000004000000000","status_code":1,"attributes":{"gen_ai.input.messages":"[{\"role\":\"user\",\"content\":\"hunter2\"}]","gen_ai.operation.name":"chat","gen_ai.output.messages":"[{\"role\":\"assistant\",\"content\":\"ok\"}]","gen_ai.request.model":"gpt-5","gen_ai.usage.cache_read.input_tokens":20,"gen_ai.usage.cost":0.0123,"gen_ai.usage.input_tokens":120,"gen_ai.usage.output_tokens":30},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":true}
{"signal":"span","session_id":"conv-1","author":"chris","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b176","parent_span_id":"eee19b7ec3c1b175","name":"retrieve","span_kind":1,"start_unix_nano":"1760000004000000000","end_unix_nano":"1760000005000000000","resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":true}
{"signal":"span","session_id":"conv-1","author":"chris","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b177","parent_span_id":"eee19b7ec3c1b176","name":"execute_tool bash","span_kind":1,"start_unix_nano":"1760000005000000000","end_unix_nano":"1760000006500000000","status_code":2,"attributes":{"gen_ai.operation.name":"execute_tool","gen_ai.tool.call.arguments":"rm -rf /","gen_ai.tool.call.id":"call_1","gen_ai.tool.name":"bash"},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":true}
`

const wantSpans = `invoke_agent helper|-|invoke_agent|ok|120|30|20|0|0.0123
chat gpt-5|invoke_agent helper|chat|ok|120|30|20|0|0.0123
execute_tool bash|chat gpt-5|execute_tool|error|0|0|0|0|null`

const wantEvents = `eee19b7ec3c1b174|conv-1|invoke_agent|9000|helper
eee19b7ec3c1b175|conv-1|chat|3000|
eee19b7ec3c1b176|conv-1|span|1000|
eee19b7ec3c1b177|conv-1|execute_tool|1500|`

var idHex = regexp.MustCompile(`"(traceId|spanId|parentSpanId)":"([0-9a-f]*)"`)

func protobufOf(t *testing.T, jsonBody string) []byte {
	t.Helper()
	b64 := idHex.ReplaceAllStringFunc(jsonBody, func(m string) string {
		sub := idHex.FindStringSubmatch(m)
		raw, err := hex.DecodeString(sub[2])
		if err != nil {
			t.Fatal(err)
		}
		return `"` + sub[1] + `":"` + base64.StdEncoding.EncodeToString(raw) + `"`
	})
	var req colltrace.ExportTraceServiceRequest
	if err := protojson.Unmarshal([]byte(b64), &req); err != nil {
		t.Fatal(err)
	}
	out, err := proto.Marshal(&req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func gzipOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

func TestReceiver(t *testing.T) {
	jsonBody := []byte(tracesJSON)
	cases := []struct {
		name         string
		contentType  string
		encoding     string
		body         []byte
		storeContent bool
		wantPartial  string
	}{
		{"json", "application/json", "", jsonBody, false, `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"1 rejected: span without a 16-byte trace_id and an 8-byte span_id"}}`},
		{"protobuf", "application/x-protobuf", "", protobufOf(t, tracesJSON), false, ""},
		{"gzip json", "application/json", "gzip", gzipOf(t, jsonBody), false, ""},
		{"json with content", "application/json", "", jsonBody, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			srv, err := server.New(dir, auth.Authors(map[string]string{auth.Hash("tok"): "chris"}))
			if err != nil {
				t.Fatal(err)
			}
			srv.StoreOTLPContent(c.storeContent)
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			post := func(body []byte) (int, string, string) {
				req, _ := http.NewRequest("POST", ts.URL+"/v1/traces", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer tok")
				req.Header.Set("Content-Type", c.contentType)
				if c.encoding != "" {
					req.Header.Set("Content-Encoding", c.encoding)
				}
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				out, _ := io.ReadAll(res.Body)
				return res.StatusCode, res.Header.Get("Content-Type"), string(out)
			}
			status, ctype, out := post(c.body)
			if status != 200 || ctype != c.contentType {
				t.Fatalf("status %d content-type %q: %s", status, ctype, out)
			}
			if c.wantPartial != "" {
				var compact bytes.Buffer
				if err := json.Compact(&compact, []byte(out)); err != nil || compact.String() != c.wantPartial {
					t.Fatalf("response %s, want %s", out, c.wantPartial)
				}
			}
			if c.contentType == "application/x-protobuf" {
				var resp colltrace.ExportTraceServiceResponse
				if err := proto.Unmarshal([]byte(out), &resp); err != nil || resp.GetPartialSuccess().GetRejectedSpans() != 1 {
					t.Fatalf("partial success %v err %v", &resp, err)
				}
			}

			want := rawNoContent
			if c.storeContent {
				want = rawWithContent
			}
			name := "otlp/agent_app/" + time.Now().UTC().Format(time.DateOnly)
			key := layout.Key{Author: "chris", Machine: store.ServerMachine, Harness: "otlp", SrcID: wire.SrcID(name)}
			if got := rawOf(t, key.Raw(dir)); got != want {
				t.Fatalf("raw:\n%s\nwant:\n%s", got, want)
			}
			if status, _, _ := post(c.body); status != 200 {
				t.Fatalf("repeat: status %d", status)
			}
			if got := rawOf(t, key.Raw(dir)); got != want {
				t.Fatalf("raw after the repeated request:\n%s", got)
			}

			if err := srv.Cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			srv.Close()
			ix, err := cache.Open(dir, cache.Options{MemoryLimit: "1GB", FlagsSQL: query.SearchFlagsSQL()})
			if err != nil {
				t.Fatal(err)
			}
			defer ix.Close()
			if got := dump(t, ix, `SELECT concat_ws('|', s.name, coalesce(p.name, '-'), s.kind, s.status, s.input_tokens, s.output_tokens,
  s.cache_read_tokens, s.cache_write_tokens, coalesce(s.cost::VARCHAR, 'null'))
FROM spans s LEFT JOIN spans p ON p.span_id = s.parent_span_id ORDER BY s.start_time`); got != wantSpans {
				t.Fatalf("spans:\n%s\nwant:\n%s", got, wantSpans)
			}
			if got := dump(t, ix, `SELECT concat_ws('|', item_id, session_id, op_kind, latency_ms, agent_label) FROM events ORDER BY event_time`); got != wantEvents {
				t.Fatalf("events:\n%s\nwant:\n%s", got, wantEvents)
			}
		})
	}
}

func rawOf(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dump(t *testing.T, ix *cache.Index, q string) string {
	t.Helper()
	rows, err := ix.DB().Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

const (
	secretTrace   = "5b8efff798038103d269b633813fc60d"
	gateTraceJSON = `{"resourceSpans":[{
  "resource":{"attributes":[
    {"key":"service.name","value":{"stringValue":"gate"}},
    {"key":"service.version","value":{"stringValue":"1.2"}},
    {"key":"deployment.note","value":{"stringValue":"RESOURCE-SECRET"}}]},
  "scopeSpans":[{"scope":{"name":"s","version":"1"},"spans":[
    {"traceId":"` + secretTrace + `","spanId":"0000000000000001","name":"ai.generateText","kind":1,
     "startTimeUnixNano":"1760000000000000000","endTimeUnixNano":"1760000001000000000",
     "status":{"code":2,"message":"STATUS-SECRET"},
     "attributes":[
       {"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},
       {"key":"gen_ai.conversation.id","value":{"stringValue":"conv-gate"}},
       {"key":"gen_ai.usage.input_tokens","value":{"intValue":5}},
       {"key":"ai.prompt.messages","value":{"stringValue":"VERCEL-SECRET"}},
       {"key":"traceloop.entity.input","value":{"stringValue":"TRACELOOP-SECRET"}},
       {"key":"app.context","value":{"kvlistValue":{"values":[{"key":"prompt","value":{"stringValue":"KVLIST-SECRET"}}]}}}],
     "events":[{"name":"exception","timeUnixNano":"1760000000500000000","attributes":[
       {"key":"exception.type","value":{"stringValue":"ValueError"}},
       {"key":"exception.message","value":{"stringValue":"EXCEPTION-SECRET"}},
       {"key":"exception.stacktrace","value":{"stringValue":"STACK-SECRET"}}]}]}
  ]}]}]}`
	gateLogsJSON = `{"resourceLogs":[{
  "resource":{"attributes":[{"key":"service.name","value":{"stringValue":"gate"}}]},
  "scopeLogs":[{"logRecords":[{"timeUnixNano":"1760000002000000000","body":{"stringValue":"BODY-SECRET"},"attributes":[
    {"key":"session.id","value":{"stringValue":"conv-gate"}},
    {"key":"event.name","value":{"stringValue":"api_request"}},
    {"key":"langfuse.observation.input","value":{"stringValue":"LOG-SECRET"}}]}]}]}]}`
)

var gateSecrets = []string{"VERCEL-SECRET", "TRACELOOP-SECRET", "KVLIST-SECRET", "EXCEPTION-SECRET", "STACK-SECRET", "STATUS-SECRET", "RESOURCE-SECRET", "BODY-SECRET", "LOG-SECRET"}

const cycleJSON = `{"resourceSpans":[{
  "resource":{"attributes":[{"key":"service.name","value":{"stringValue":"loops"}}]},
  "scopeSpans":[{"spans":[
    {"traceId":"5b8efff798038103d269b633813fc60e","spanId":"0000000000000001","parentSpanId":"0000000000000001","name":"self","kind":1,
     "startTimeUnixNano":"1760000000000000000","endTimeUnixNano":"1760000001000000000",
     "attributes":[{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},{"key":"gen_ai.conversation.id","value":{"stringValue":"conv-loop"}}]},
    {"traceId":"5b8efff798038103d269b633813fc60e","spanId":"0000000000000003","parentSpanId":"0000000000000002","name":"c","kind":1,
     "startTimeUnixNano":"1760000002000000000","endTimeUnixNano":"1760000003000000000",
     "attributes":[{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},{"key":"gen_ai.conversation.id","value":{"stringValue":"conv-loop"}}]},
    {"traceId":"5b8efff798038103d269b633813fc60e","spanId":"0000000000000002","parentSpanId":"0000000000000003","name":"b","kind":1,
     "startTimeUnixNano":"1760000004000000000","endTimeUnixNano":"1760000005000000000",
     "attributes":[{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},{"key":"gen_ai.conversation.id","value":{"stringValue":"conv-loop"}}]}
  ]}]}]}`

func startServer(t *testing.T, tokens map[string]string, storeContent bool) (string, *server.Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	byHash := map[string]string{}
	for tok, author := range tokens {
		byHash[auth.Hash(tok)] = author
	}
	srv, err := server.New(dir, auth.Authors(byHash))
	if err != nil {
		t.Fatal(err)
	}
	srv.StoreOTLPContent(storeContent)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return dir, srv, ts
}

func postJSON(t *testing.T, ts *httptest.Server, path, token string, body io.Reader) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(out)
}

func converge(t *testing.T, dir string, srv *server.Server) *cache.Index {
	t.Helper()
	if err := srv.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	ix, err := cache.Open(dir, cache.Options{MemoryLimit: "1GB", FlagsSQL: query.SearchFlagsSQL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	return ix
}

func TestReceiverRows(t *testing.T) {
	day := time.Now().UTC().Format(time.DateOnly)

	for _, content := range []bool{false, true} {
		name := "gate without content"
		if content {
			name = "gate with content"
		}
		t.Run(name, func(t *testing.T) {
			dir, _, ts := startServer(t, map[string]string{"tok": "chris"}, content)
			if status, _, out := postJSON(t, ts, "/v1/traces", "tok", strings.NewReader(gateTraceJSON)); status != 200 {
				t.Fatalf("traces: %d %s", status, out)
			}
			if status, _, out := postJSON(t, ts, "/v1/logs", "tok", strings.NewReader(gateLogsJSON)); status != 200 {
				t.Fatalf("logs: %d %s", status, out)
			}
			key := layout.Key{Author: "chris", Machine: store.ServerMachine, Harness: "otlp", SrcID: wire.SrcID("otlp/gate/" + day)}
			raw := rawOf(t, key.Raw(dir))
			for _, s := range gateSecrets {
				if got := strings.Contains(raw, s); got != content {
					t.Errorf("content_stored=%v: raw holds %s = %v", content, s, got)
				}
			}
			for _, kept := range []string{`"gen_ai.usage.input_tokens":5`, `"exception.type":"ValueError"`, `"service.version":"1.2"`, `"service.name":"gate"`} {
				if !strings.Contains(raw, kept) {
					t.Errorf("raw lost %s:\n%s", kept, raw)
				}
			}
		})
	}

	t.Run("two authors, one span", func(t *testing.T) {
		dir, srv, ts := startServer(t, map[string]string{"tokA": "alice", "tokB": "bob"}, false)
		for _, tok := range []string{"tokA", "tokB"} {
			if status, _, out := postJSON(t, ts, "/v1/traces", tok, strings.NewReader(tracesJSON)); status != 200 {
				t.Fatalf("%s: %d %s", tok, status, out)
			}
		}
		ix := converge(t, dir, srv)
		const q = `SELECT concat_ws('|', author, count(*)) FROM events WHERE item_id = 'eee19b7ec3c1b175' GROUP BY author ORDER BY author`
		if got, want := dump(t, ix, q), "alice|1\nbob|1"; got != want {
			t.Fatalf("events of one span id:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("span parent loops", func(t *testing.T) {
		dir, srv, ts := startServer(t, map[string]string{"tok": "chris"}, false)
		if status, _, out := postJSON(t, ts, "/v1/traces", "tok", strings.NewReader(cycleJSON)); status != 200 {
			t.Fatalf("%d %s", status, out)
		}
		ix := converge(t, dir, srv)
		const q = `SELECT concat_ws('|', s.name, coalesce(p.name, '-')) FROM spans s LEFT JOIN spans p ON p.span_id = s.parent_span_id ORDER BY s.name`
		if got, want := dump(t, ix, q), "b|-\nc|b\nself|-"; got != want {
			t.Fatalf("spans:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("decodes beyond the slots", func(t *testing.T) {
		_, _, ts := startServer(t, map[string]string{"tok": "chris"}, false)
		type held struct {
			w    *io.PipeWriter
			done chan int
		}
		var holds []held
		for i := 0; i < otlp.DecodeSlots; i++ {
			pr, pw := io.Pipe()
			h := held{pw, make(chan int, 1)}
			go func() {
				req, _ := http.NewRequest("POST", ts.URL+"/v1/traces", pr)
				req.Header.Set("Authorization", "Bearer tok")
				req.Header.Set("Content-Type", "application/json")
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Logf("held request: %v", err)
					h.done <- -1
					return
				}
				res.Body.Close()
				h.done <- res.StatusCode
			}()
			holds = append(holds, h)
			t.Cleanup(func() { pw.Close() })
		}
		time.Sleep(time.Second) // the client waits 200 ms for a first byte before it sends headers of a streamed body
		var status int
		var header http.Header
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if status, header, _ = postJSON(t, ts, "/v1/traces", "tok", strings.NewReader(`{}`)); status == http.StatusTooManyRequests {
				break
			}
		}
		if status != http.StatusTooManyRequests || header.Get("Retry-After") != "1" {
			t.Fatalf("status %d Retry-After %q with every slot held, want 429 and 1", status, header.Get("Retry-After"))
		}
		for _, h := range holds {
			h.w.Write([]byte(`{}`))
			h.w.Close()
			if got := <-h.done; got != 200 {
				t.Fatalf("held request: status %d", got)
			}
		}
		if status, _, _ := postJSON(t, ts, "/v1/traces", "tok", strings.NewReader(`{}`)); status != 200 {
			t.Fatalf("after release: status %d", status)
		}
	})
}
