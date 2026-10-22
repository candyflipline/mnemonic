package cache

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
)

func TestClaudeCodeSpanTree(t *testing.T) {
	dataDir := t.TempDir()
	if err := cachetest.Lake(dataDir, "claude_code"); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if _, err := ix.Converge(context.Background(), RetryFailed); err != nil {
		t.Fatal(err)
	}
	rows, err := ix.DB().Query(`SELECT concat_ws(' ', span_id, coalesce(nullif(parent_span_id, ''), '-'), kind, name,
  status, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, coalesce(cost::VARCHAR, 'null'),
  first_event_uid[1:8], last_event_uid[1:8])
FROM spans WHERE session_id = '7c666c01-d38e-4658-8650-854ffb5b626e' ORDER BY start_time, kind`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"5c1265bbc73a071b - invoke_agent claude-code ok 9 5 2 19630 null 3e2f31f3 30afddf1",
		"055bfbac1f915da7 5c1265bbc73a071b turn turn 1 ok 9 5 2 19630 null 3e2f31f3 30afddf1",
		"93d47cb9e47a4461 055bfbac1f915da7 chat claude-opus-4-5-20251101 ok 9 5 2 19630 null 8814c5f2 d18fa55c",
		"712db1ba0f646f0e 93d47cb9e47a4461 execute_tool Read ok 0 0 0 0 null b7a60c19 8e640ed7",
	}, "\n")
	if g := strings.Join(got, "\n"); g != want {
		t.Fatalf("spans:\n%s\nwant:\n%s", g, want)
	}
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

func spanRows(t *testing.T, ix *Index) []spanRow {
	t.Helper()
	rows, err := ix.DB().Query(`SELECT span_id, parent_span_id, kind, name, status,
  strftime(start_time, '%Y-%m-%dT%H:%M:%S.%g'), strftime(end_time, '%Y-%m-%dT%H:%M:%S.%g'),
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, first_event_uid[1:8], last_event_uid[1:8]
FROM spans ORDER BY session_id, start_time, kind, span_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []spanRow
	for rows.Next() {
		var r spanRow
		if err := rows.Scan(&r.SpanID, &r.ParentSpanID, &r.Kind, &r.Name, &r.Status, &r.Start, &r.End,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.FirstEvent, &r.LastEvent); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestHarnessSpanTrees(t *testing.T) {
	for _, golden := range []string{"codex", "cursor", "cursor_sqlite", "pi_coding_agent", "prime_agent"} {
		t.Run(golden, func(t *testing.T) {
			dataDir := t.TempDir()
			if err := cachetest.Lake(dataDir, golden); err != nil {
				t.Fatal(err)
			}
			open := func() *Index {
				ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
				if err != nil {
					t.Fatal(err)
				}
				return ix
			}
			ix := open()
			if _, err := ix.Converge(context.Background(), RetryFailed); err != nil {
				t.Fatal(err)
			}
			got := spanRows(t, ix)

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

			var calls, tools int
			if err := ix.DB().QueryRow(`SELECT (SELECT count(DISTINCT tool_call_id) FROM events WHERE tool_call_id <> ''),
  (SELECT count(*) FROM spans WHERE kind = 'execute_tool')`).Scan(&calls, &tools); err != nil {
				t.Fatal(err)
			}
			if calls != tools {
				t.Fatalf("%d tool_call_ids, %d execute_tool spans", calls, tools)
			}

			if _, err := ix.DB().Exec("DELETE FROM spans; UPDATE meta SET spans_fingerprint = ''"); err != nil {
				t.Fatal(err)
			}
			ix.Close()
			ix = open()
			defer ix.Close()
			if n := len(spanRows(t, ix)); n != 0 {
				t.Fatalf("%d spans before the re-derive, want 0", n)
			}
			if _, err := ix.Converge(context.Background(), RetryFailed); err != nil {
				t.Fatal(err)
			}
			if again := spanRows(t, ix); !reflect.DeepEqual(again, got) {
				t.Fatalf("spans changed on re-derive:\n%v\nwas:\n%v", again, got)
			}
		})
	}
}

func TestReorderQueuesEachSessionOnce(t *testing.T) {
	ix, err := Open(t.TempDir(), Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	ctx := context.Background()
	c, err := ix.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "CREATE TEMP TABLE s AS SELECT unnest(['a', 'b']) AS session_id"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := reorder(ctx, c, &ConvergeStats{}); err != nil {
			t.Fatal(err)
		}
	}
	var queued, distinct int
	if err := c.QueryRowContext(ctx, "SELECT count(*), count(DISTINCT session_id) FROM pending_spans").Scan(&queued, &distinct); err != nil {
		t.Fatal(err)
	}
	if queued != 2 || distinct != 2 {
		t.Fatalf("pending_spans holds %d rows for %d sessions after three reorders, want 2 and 2", queued, distinct)
	}
}
