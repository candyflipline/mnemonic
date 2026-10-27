package export

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/index"
)

const linkSessionID = "11111111-2222-3333-4444-555555555555"

func linkDB(t *testing.T, lines []string) *index.DB {
	t.Helper()
	root, store := t.TempDir(), t.TempDir()
	writeRawSource(t, root, "test", "claude-code", "claude-code/link.jsonl", []byte(strings.Join(lines, "\n")+"\n"))
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
	return db
}

func linkView(cutoff time.Time) LinkView {
	return LinkView{
		Title:       "QUIC context",
		SessionIDs:  []string{linkSessionID},
		Cutoff:      cutoff,
		MarkdownURL: "https://share.example/s/tok.md",
		SessionURL: func(n int, ext string) string {
			return fmt.Sprintf("https://share.example/s/tok/%d.%s", n, ext)
		},
	}
}

func record(typ, uuid, ts, extra, content string) string {
	return fmt.Sprintf(`{"type":%q,"sessionId":%q,"cwd":"/work/demo","uuid":%q,"timestamp":%q%s,"message":{"role":%q,"content":%s}}`,
		typ, linkSessionID, uuid, ts, extra, typ, content)
}

func toolUse(uuid, ts, extra, id, name, input string) string {
	return record("assistant", uuid, ts, extra, `[{"type":"tool_use","id":"`+id+`","name":"`+name+`","input":`+input+`}]`)
}

func toolResult(uuid, ts, extra, id, text string) string {
	return record("user", uuid, ts, extra, `[{"type":"tool_result","tool_use_id":"`+id+`","content":`+text+`}]`)
}

func workLines(steps []*step) []string {
	var out []string
	for _, s := range steps {
		switch {
		case s.Kind != stepTool:
			out = append(out, string(s.Kind)+": "+s.Text)
		case s.Call.Sub != nil:
			sub := s.Call.Sub
			out = append(out, fmt.Sprintf("subagent %s: %s | %s | %s", s.Call.Summary, sub.Prompt, strings.Join(workLines(sub.Steps), "; "), sub.Report))
		default:
			out = append(out, fmt.Sprintf("tool %s: %s -> %s", s.Call.Name, s.Call.Summary, s.Call.Output))
		}
	}
	return out
}

func TestLinkTurns(t *testing.T) {
	sub := `,"isSidechain":true,"agentId":"a1b2c3"`
	human := `,"origin":{"kind":"human"}`
	db := linkDB(t, []string{
		`{"type":"queue-operation","operation":"enqueue","sessionId":"` + linkSessionID + `","timestamp":"2026-10-27T10:00:00.000Z","content":"List the files."}`,
		record("user", "u1", "2026-10-27T10:00:00.010Z", human, `"List the files.<system-reminder>be brief</system-reminder>"`),
		record("user", "u2", "2026-10-27T10:00:00.020Z", `,"isMeta":true`, `[{"type":"text","text":"Base directory for this skill: /skills/poteto\n\n# Poteto mode"}]`),
		toolUse("a1", "2026-10-27T10:00:01.000Z", "", "t1", "Bash", `{"command":"ls","description":"List files"}`),
		toolResult("u3", "2026-10-27T10:00:02.000Z", "", "t1", `"a.go\nb.go"`),
		toolUse("a2", "2026-10-27T10:00:03.000Z", "", "t2", "AskUserQuestion", `{"questions":[{"question":"Ship it?","options":[{"label":"Yes"},{"label":"No"}]}]}`),
		toolResult("u4", "2026-10-27T10:00:04.000Z", "", "t2", `"The user answered: \"Ship it?\"=\"Yes\". You can now continue."`),
		toolUse("a3", "2026-10-27T10:00:05.000Z", "", "t3", "Agent", `{"description":"Find tests","prompt":"Find the tests."}`),
		toolResult("u5", "2026-10-27T10:00:06.000Z", "", "t3", `"Async agent launched.\nagentId: a1b2c3 (internal ID)"`),
		record("user", "s1", "2026-10-27T10:00:07.000Z", sub, `"Find the tests."`),
		toolUse("s2", "2026-10-27T10:00:08.000Z", sub, "t4", "Grep", `{"pattern":"func Test"}`),
		toolResult("s3", "2026-10-27T10:00:09.000Z", sub, "t4", `"x_test.go"`),
		record("assistant", "s4", "2026-10-27T10:00:10.000Z", sub, `[{"type":"text","text":"One test file."}]`),
		record("assistant", "a4", "2026-10-27T10:00:11.000Z", "", `[{"type":"text","text":"Checking."}]`),
		record("assistant", "a5", "2026-10-27T10:00:12.000Z", "", `[{"type":"text","text":"Two files; tests started."}]`),
		`{"type":"system","subtype":"away_summary","sessionId":"` + linkSessionID + `","uuid":"y1","timestamp":"2026-10-27T10:00:13.000Z","content":"You asked for files."}`,
		record("user", "u6", "2026-10-27T10:00:20.000Z", `,"origin":{"kind":"task-notification"}`, `"<task-notification>\n<summary>Agent \"Find tests\" finished</summary>\n</task-notification>"`),
		record("assistant", "a6", "2026-10-27T10:00:21.000Z", "", `[{"type":"text","text":"It found one test file."}]`),
	})
	ls, err := openLinkSession(db, linkView(time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)), 1)
	if err != nil {
		t.Fatal(err)
	}
	type turnWant struct {
		trigger triggerKind
		prompt  string
		asks    []qa
		work    []string
		summary string
		answer  string
	}
	want := []turnWant{
		{
			trigger: triggerHuman, prompt: "List the files.",
			asks: []qa{{"Ship it?", "Yes"}},
			work: []string{
				"notice: Loaded skill poteto",
				"tool Bash: List files -> a.go\nb.go",
				`tool AskUserQuestion:  -> The user answered: "Ship it?"="Yes". You can now continue.`,
				"subagent Find tests: Find the tests. | tool Grep: func Test -> x_test.go | One test file.",
				"note: Checking.",
			},
			summary: "Worked 11 s · 3 tool calls · 1 subagent",
			answer:  "Two files; tests started.",
		},
		{trigger: triggerBackground, prompt: `Agent "Find tests" finished`, summary: "Worked 1 s · 0 tool calls", answer: "It found one test file."},
	}
	var got []turnWant
	for _, tr := range ls.th.Turns {
		got = append(got, turnWant{tr.Trigger, tr.Prompt, tr.Asks, workLines(tr.Work), tr.WorkSummary(), tr.Answer})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("turns:\n%#v\nwant:\n%#v", got, want)
	}
}

func TestLinkMarkdownDropsUnstamped(t *testing.T) {
	db := linkDB(t, []string{
		record("user", "u1", "2026-10-27T10:00:00.000Z", "", `"List the files."`),
		record("user", "u3", "2026-10-27T10:05:00.000Z", "", `"Added after the share."`),
		record("user", "u4", "not a time", "", `"A line with no time."`),
	})
	var b bytes.Buffer
	if err := WriteSessionMarkdown(&b, db, linkView(time.Date(2026, 10, 27, 10, 10, 0, 0, time.UTC)), 1); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); !strings.Contains(got, "Added after the share.") || strings.Contains(got, "A line with no time.") {
		t.Fatalf("session markdown:\n%s\nwant the 10:05 line and not the unstamped one", got)
	}
}

func TestRenderMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<script>alert(1)</script> & "q"`, "<p>&lt;script&gt;alert(1)&lt;/script&gt; &amp; &#34;q&#34;</p>\n"},
		{"[x](javascript:alert(1)) [y](https://a.example/?q=1&r=2)",
			`<p>[x](javascript:alert(1)) <a href="https://a.example/?q=1&amp;r=2" rel="noreferrer noopener">y</a></p>` + "\n"},
		{"see https://a.example/x.",
			`<p>see <a href="https://a.example/x" rel="noreferrer noopener">https://a.example/x</a>.</p>` + "\n"},
		{"| a | b |\n|---|:-:|\n| `<i>` | **x** |",
			"<div class=\"table\"><table>\n<tr><th>a</th><th>b</th></tr>\n<tr><td><code>&lt;i&gt;</code></td><td><strong>x</strong></td></tr>\n</table></div>\n"},
		{"## Hi *there*\n- one\n- two_three\n\n```go\n<b>\n```",
			"<h4>Hi <em>there</em></h4>\n<ul>\n<li>one</li>\n<li>two_three</li>\n</ul>\n<pre><code>&lt;b&gt;</code></pre>\n"},
	}
	for _, c := range cases {
		if got := string(renderMarkdown(c.in)); got != c.want {
			t.Errorf("renderMarkdown(%q):\n%s\nwant:\n%s", c.in, got, c.want)
		}
	}
}

func TestLinkAgentIDOnlyFromAgentCalls(t *testing.T) {
	sub := `,"isSidechain":true,"agentId":"a1b2c3"`
	human := `,"origin":{"kind":"human"}`
	db := linkDB(t, []string{
		record("user", "u1", "2026-10-27T10:00:00.000Z", human, `"Find tests."`),
		toolUse("a1", "2026-10-27T10:00:01.000Z", "", "t1", "Agent", `{"description":"Find tests","prompt":"Find the tests."}`),
		toolResult("u2", "2026-10-27T10:00:02.000Z", "", "t1", `"Async agent launched.\nagentId: a1b2c3 (internal ID)"`),
		record("user", "s1", "2026-10-27T10:00:03.000Z", sub, `"Find the tests."`),
		record("assistant", "s2", "2026-10-27T10:00:04.000Z", sub, `[{"type":"text","text":"One test file."}]`),
		record("assistant", "a2", "2026-10-27T10:00:05.000Z", "", `[{"type":"text","text":"Started."}]`),
		record("user", "u3", "2026-10-27T10:01:00.000Z", human, `"Check on it."`),
		toolUse("a3", "2026-10-27T10:01:01.000Z", "", "t2", "Bash", `{"command":"cat log","description":"Read the log"}`),
		toolResult("u4", "2026-10-27T10:01:02.000Z", "", "t2", `"started agentId: a1b2c3 earlier"`),
		record("assistant", "a4", "2026-10-27T10:01:03.000Z", "", `[{"type":"text","text":"Done."}]`),
	})
	for i := 0; i < 20; i++ {
		ls, err := openLinkSession(db, linkView(time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)), 1)
		if err != nil {
			t.Fatal(err)
		}
		got := [][]string{workLines(ls.th.Turns[0].Work), workLines(ls.th.Turns[1].Work)}
		want := [][]string{
			{"subagent Find tests: Find the tests. |  | One test file."},
			{"tool Bash: Read the log -> started agentId: a1b2c3 earlier"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("render %d work:\n%#v\nwant:\n%#v", i, got, want)
		}
	}
}

func TestLinkHumanPromptKeepsTags(t *testing.T) {
	human := `,"origin":{"kind":"human"}`
	db := linkDB(t, []string{
		record("user", "u1", "2026-10-27T10:00:00.000Z", human, `"<div>pasted snippet</div>\nwhy is this broken?"`),
		record("assistant", "a1", "2026-10-27T10:00:01.000Z", "", `[{"type":"text","text":"Unclosed."}]`),
		record("user", "u2", "2026-10-27T10:00:02.000Z", human, `"<b>only a tag</b>"`),
		record("assistant", "a2", "2026-10-27T10:00:03.000Z", "", `[{"type":"text","text":"Bold."}]`),
		record("user", "u3", "2026-10-27T10:00:04.000Z", human, `"<system-reminder>x</system-reminder>kept words"`),
	})
	ls, err := openLinkSession(db, linkView(time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)), 1)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tr := range ls.th.Turns {
		got = append(got, tr.Prompt)
	}
	want := []string{"<div>pasted snippet</div>\nwhy is this broken?", "<b>only a tag</b>", "kept words"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prompts:\n%q\nwant:\n%q", got, want)
	}
}

func TestRenderMarkdownEmphasisIsLinear(t *testing.T) {
	for _, unit := range []string{"_a <b> ", "*a <b> "} {
		src := strings.Repeat(unit, 900<<10/len(unit))
		start := time.Now()
		out := string(renderMarkdown(src))
		if d := time.Since(start); d > time.Second {
			t.Errorf("renderMarkdown of %d KB of %q took %v, want under 1s", len(src)>>10, unit, d)
		}
		if strings.Contains(out, "<b>") || !strings.Contains(out, "&lt;b&gt;") {
			t.Errorf("output for %q is not escaped: %.80s", unit, out)
		}
	}
}
