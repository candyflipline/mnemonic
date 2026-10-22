package cache

import (
	"context"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/cache/cachetest"
	"github.com/candyflipline/mnemonic/internal/layout"
)

func TestSessionAuthorsIncrementalMatchesRebuild(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	if err := cachetest.Source(dataDir, layout.Key{Author: "alice"}, "claude_code"); err != nil {
		t.Fatal(err)
	}
	ix, err := Open(dataDir, Options{MemoryLimit: "1GB", FlagsSQL: "0::USMALLINT"})
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	authors := func() string {
		rows, err := ix.DB().Query("SELECT session_id || ' ' || author FROM session_authors ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return strings.Join(out, ", ")
	}
	const session = "7c666c01-d38e-4658-8650-854ffb5b626e"
	if _, err := ix.Converge(ctx, RetryFailed); err != nil {
		t.Fatal(err)
	}
	if got, want := authors(), session+" alice"; got != want {
		t.Fatalf("after alice: %s, want %s", got, want)
	}
	if err := cachetest.Source(dataDir, layout.Key{Author: "bob"}, "claude_code"); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Converge(ctx, RetryFailed); err != nil {
		t.Fatal(err)
	}
	want := session + " alice, " + session + " bob"
	if got := authors(); got != want {
		t.Fatalf("incremental: %s, want %s", got, want)
	}
	if _, err := ix.DB().Exec("UPDATE meta SET session_authors_built = false"); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Converge(ctx, RetryFailed); err != nil {
		t.Fatal(err)
	}
	if got := authors(); got != want {
		t.Fatalf("rebuild: %s, want %s", got, want)
	}
}
