package export

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/index"
)

const linkSessionID = "11111111-2222-3333-4444-555555555555"

func linkFixture(t *testing.T) (*index.DB, LinkView) {
	t.Helper()
	line := func(typ, uuid, ts, content string) string {
		return fmt.Sprintf(`{"type":%q,"sessionId":%q,"cwd":"/work/demo","uuid":%q,"requestId":"req-%s","timestamp":%q,"message":{"role":%q,"content":%s}}`,
			typ, linkSessionID, uuid, uuid, ts, typ, content)
	}
	lines := strings.Join([]string{
		line("user", "u1", "2026-10-27T10:00:00.000Z", `"List the files."`),
		line("assistant", "a1", "2026-10-27T10:00:01.000Z",
			`[{"type":"thinking","thinking":"private thoughts"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]`),
		line("user", "u2", "2026-10-27T10:00:02.000Z",
			`[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"`+strings.Repeat("x", 5000)+`"}]}]`),
		line("assistant", "a2", "2026-10-27T10:00:03.000Z", `[{"type":"text","text":"Five thousand x."}]`),
		line("user", "u3", "2026-10-27T10:05:00.000Z", `"Added after the share."`),
	}, "\n") + "\n"
	root, store := t.TempDir(), t.TempDir()
	writeRawSource(t, root, "test", "claude-code", "claude-code/link.jsonl", []byte(lines))
	if _, err := index.Ingest(root, store, time.Minute, true); err != nil {
		t.Fatal(err)
	}
	ix, err := index.Open(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	db, release := ix.Snapshot()
	t.Cleanup(release)
	return db, LinkView{
		Title:       "QUIC context",
		Note:        "Read this first.",
		SessionIDs:  []string{linkSessionID},
		Cutoff:      time.Date(2026, 10, 27, 10, 1, 0, 0, time.UTC),
		MarkdownURL: "https://share.example/s/tok.md",
		SessionURL: func(n int, ext string) string {
			return fmt.Sprintf("https://share.example/s/tok/%d.%s", n, ext)
		},
	}
}

func TestLinkMarkdown(t *testing.T) {
	db, v := linkFixture(t)
	var b bytes.Buffer
	if err := WriteSessionMarkdown(&b, db, v, 1); err != nil {
		t.Fatal(err)
	}
	want := "# Session 1: claude-code, demo, 2026-10-27 03:00 PDT, 3 steps\n\n" +
		"### User\n\nList the files.\n\n" +
		"### Assistant\n\nTool call: Bash {\"command\":\"ls\"}\n\n" +
		"Tool result of Bash:\n\n```\n" + strings.Repeat("x", 4096) + "\n```\n\n" +
		"Output cut at 4 KB of 5000 bytes. The full output is at https://share.example/s/tok/1.json\n\n" +
		"### Assistant\n\nFive thousand x.\n\n"
	if b.String() != want {
		t.Fatalf("session markdown:\n%s\nwant:\n%s", b.String(), want)
	}
	if err := WriteSessionMarkdown(&b, db, v, 2); err == nil {
		t.Fatal("session 2 of a one-session link was written")
	}
}
