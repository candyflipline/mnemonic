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

const rawNoContent = `{"signal":"span","session_id":"conv-1","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b174","name":"invoke_agent helper","span_kind":1,"start_unix_nano":"1760000000000000000","end_unix_nano":"1760000009000000000","status_code":1,"attributes":{"gen_ai.agent.name":"helper","gen_ai.conversation.id":"conv-1","gen_ai.operation.name":"invoke_agent"},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":false}
{"signal":"span","session_id":"conv-1","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b175","parent_span_id":"eee19b7ec3c1b174","name":"chat gpt-5","span_kind":3,"start_unix_nano":"1760000001000000000","end_unix_nano":"1760000004000000000","status_code":1,"attributes":{"gen_ai.operation.name":"chat","gen_ai.request.model":"gpt-5","gen_ai.usage.cache_read.input_tokens":20,"gen_ai.usage.cost":0.0123,"gen_ai.usage.input_tokens":120,"gen_ai.usage.output_tokens":30},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":false}
{"signal":"span","session_id":"conv-1","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b176","parent_span_id":"eee19b7ec3c1b175","name":"retrieve","span_kind":1,"start_unix_nano":"1760000004000000000","end_unix_nano":"1760000005000000000","resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":false}
{"signal":"span","session_id":"conv-1","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b177","parent_span_id":"eee19b7ec3c1b176","name":"execute_tool bash","span_kind":1,"start_unix_nano":"1760000005000000000","end_unix_nano":"1760000006500000000","status_code":2,"attributes":{"gen_ai.operation.name":"execute_tool","gen_ai.tool.call.id":"call_1","gen_ai.tool.name":"bash"},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":false}
`

const rawWithContent = `{"signal":"span","session_id":"conv-1","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b174","name":"invoke_agent helper","span_kind":1,"start_unix_nano":"1760000000000000000","end_unix_nano":"1760000009000000000","status_code":1,"attributes":{"gen_ai.agent.name":"helper","gen_ai.conversation.id":"conv-1","gen_ai.operation.name":"invoke_agent"},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":true}
{"signal":"span","session_id":"conv-1","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b175","parent_span_id":"eee19b7ec3c1b174","name":"chat gpt-5","span_kind":3,"start_unix_nano":"1760000001000000000","end_unix_nano":"1760000004000000000","status_code":1,"attributes":{"gen_ai.input.messages":"[{\"role\":\"user\",\"content\":\"hunter2\"}]","gen_ai.operation.name":"chat","gen_ai.output.messages":"[{\"role\":\"assistant\",\"content\":\"ok\"}]","gen_ai.request.model":"gpt-5","gen_ai.usage.cache_read.input_tokens":20,"gen_ai.usage.cost":0.0123,"gen_ai.usage.input_tokens":120,"gen_ai.usage.output_tokens":30},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":true}
{"signal":"span","session_id":"conv-1","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b176","parent_span_id":"eee19b7ec3c1b175","name":"retrieve","span_kind":1,"start_unix_nano":"1760000004000000000","end_unix_nano":"1760000005000000000","resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":true}
{"signal":"span","session_id":"conv-1","trace_id":"5b8efff798038103d269b633813fc60c","span_id":"eee19b7ec3c1b177","parent_span_id":"eee19b7ec3c1b176","name":"execute_tool bash","span_kind":1,"start_unix_nano":"1760000005000000000","end_unix_nano":"1760000006500000000","status_code":2,"attributes":{"gen_ai.operation.name":"execute_tool","gen_ai.tool.call.arguments":"rm -rf /","gen_ai.tool.call.id":"call_1","gen_ai.tool.name":"bash"},"resource":{"service.name":"Agent App"},"scope":{"name":"demo","version":"1"},"content_stored":true}
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
			srv, err := server.New(dir, map[string]string{auth.Hash("tok"): "chris"})
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
