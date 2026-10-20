package all_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/candyflipline/mnemonic/internal/normalize"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
)

var cases = []struct {
	name, harness, sourceName, fixture, sourceFile string
}{
	{"codex", "codex", "golden-codex", "codex/session.jsonl", "/fixtures/codex/session.jsonl"},
	{"claude_code", "claude-code", "golden-claude-code", "claude-code/session.jsonl", "/fixtures/claude-code/session.jsonl"},
	{"kimi_cli", "kimi-cli", "golden-kimi-cli", "kimi-cli/wire.jsonl", "/fixtures/kimi-cli/wire.jsonl"},
	{"qwen_code", "qwen-code", "golden-qwen-code", "qwen-code/session.jsonl", "/fixtures/qwen-code/session.jsonl"},
	{"kiro_cli", "kiro-cli", "golden-kiro-cli", "kiro/session.jsonl", "/fixtures/kiro/11111111-2222-4333-8444-555555555555.jsonl"},
	{"opencode", "opencode", "golden-opencode", "opencode/session.jsonl", "/fixtures/opencode/session.jsonl"},
	{"cursor", "cursor", "golden-cursor",
		"cursor/projects/demo/agent-transcripts/11111111-2222-4333-8444-555555555555/11111111-2222-4333-8444-555555555555.jsonl",
		"/fixtures/cursor/projects/demo/agent-transcripts/11111111-2222-4333-8444-555555555555/11111111-2222-4333-8444-555555555555.jsonl"},
	{"hermes_trajectory", "hermes", "golden-hermes-trajectory", "hermes/trajectories/001.jsonl", "/fixtures/hermes/trajectories/001.jsonl"},
	{"pi_coding_agent", "pi-coding-agent", "golden-pi", "pi/session.jsonl", "/fixtures/pi/session.jsonl"},
	{"prime_agent", "prime-agent", "golden-prime-agent", "prime-agent/session.jsonl", "/fixtures/prime-agent/session.jsonl"},
}

func TestGoldens(t *testing.T) {
	ran := 0
	for _, c := range cases {
		p, ok := normalize.For(c.harness)
		if !ok {
			continue
		}
		ran++
		t.Run(c.name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("../../../testdata/fixtures", c.fixture))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			events, err := p.Parse(f, normalize.SourceMeta{
				SourceName: c.sourceName, Harness: c.harness, SourceFile: c.sourceFile, Generation: 1, Inode: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := roundTrip(t, events)

			var golden struct {
				Records []struct {
					EventRows []map[string]any `json:"event_rows"`
				} `json:"records"`
			}
			raw, err := os.ReadFile(filepath.Join("../../../testdata/goldens", c.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &golden); err != nil {
				t.Fatal(err)
			}
			var want []map[string]any
			for _, r := range golden.Records {
				want = append(want, r.EventRows...)
			}

			if len(got) != len(want) {
				t.Fatalf("got %d events, golden has %d", len(got), len(want))
			}
			for i := range want {
				got[i]["event_version"], want[i]["event_version"] = "<event_version>", "<event_version>"
				for k, w := range want[i] {
					if !reflect.DeepEqual(got[i][k], w) {
						t.Errorf("event %d (%v) %s: got %#v, golden %#v", i, want[i]["event_kind"], k, got[i][k], w)
					}
				}
			}
		})
	}
	t.Logf("%d of %d golden cases have a parser", ran, len(cases))
}

func roundTrip(t *testing.T, v any) []map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
