package cache

import (
	"context"
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
	rows, err := ix.DB().Query(`SELECT concat_ws(' ', tenant, span_id, coalesce(nullif(parent_span_id, ''), '-'), kind, name,
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
		"default 5c1265bbc73a071b - invoke_agent claude-code ok 9 5 2 19630 null 3e2f31f3 30afddf1",
		"default 055bfbac1f915da7 5c1265bbc73a071b turn turn 1 ok 9 5 2 19630 null 3e2f31f3 30afddf1",
		"default 93d47cb9e47a4461 055bfbac1f915da7 chat claude-opus-4-5-20251101 ok 9 5 2 19630 null 8814c5f2 d18fa55c",
		"default 712db1ba0f646f0e 93d47cb9e47a4461 execute_tool Read ok 0 0 0 0 null b7a60c19 8e640ed7",
	}, "\n")
	if g := strings.Join(got, "\n"); g != want {
		t.Fatalf("spans:\n%s\nwant:\n%s", g, want)
	}
}
