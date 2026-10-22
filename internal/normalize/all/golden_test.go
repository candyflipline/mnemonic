package all_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/candyflipline/mnemonic/internal/normalize"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/schema"
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
		if _, ok := normalize.For(c.harness); !ok {
			continue
		}
		ran++
		t.Run(c.name, func(t *testing.T) {
			got := parse(t, c.harness, c.sourceName, c.fixture, c.sourceFile)

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
				want[i]["event_version"] = "<event_version>"
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

var outputs = map[string]map[int]string{
	"claude-code": {
		1: "1196b33f5c9632a89c92362976658fcafeb3cac96096cd13da38fd250b4d1641",
		2: "1196b33f5c9632a89c92362976658fcafeb3cac96096cd13da38fd250b4d1641",
	},
	"codex":           {1: "7100d21459a5e6fa8452a3960ba0f893df7cbbe1e27a6d0150ae9aa2c2c2618e"},
	"cursor":          {1: "9d90adf0d392b99c4016b555ccfb4f600f0d6dd62b2f3a825db023039e3837c6"},
	"pi-coding-agent": {1: "c603e1ffaf1507915c244803f0eafd2dc589ca16a9fd0683df7a53b3d6ff7c8f"},
	"prime-agent":     {1: "cd56bccacb162b506ec067980b4233f5c3ad7faf802abfb2245e665c7b5315ee"},
}

func TestVersionNamesOutput(t *testing.T) {
	next := 1
	for _, vs := range outputs {
		for v := range vs {
			next = max(next, v+1)
		}
	}
	for _, c := range cases {
		p, ok := normalize.For(c.harness)
		if !ok {
			continue
		}
		b, err := json.Marshal(parse(t, c.harness, c.sourceName, c.fixture, c.sourceFile))
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		fmt.Fprintf(h, "%s\n%s\n", c.name, b)
		sum := hex.EncodeToString(h.Sum(nil))
		switch want, ok := outputs[c.harness][p.Version()]; {
		case !ok:
			t.Errorf("add %d: %q to outputs[%q]", p.Version(), sum, c.harness)
		case want != sum:
			t.Errorf("%s output changed without a Version bump: set its Version to %d and add %d: %q to outputs[%q]; never edit an entry",
				c.harness, next, next, sum, c.harness)
		}
	}
}

func parse(t *testing.T, harness, sourceName, fixture, sourceFile string) []map[string]any {
	t.Helper()
	p, _ := normalize.For(harness)
	f, err := os.Open(filepath.Join("../../../testdata/fixtures", fixture))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []schema.Event
	_, err = p.Parse(f, normalize.SourceMeta{SourceName: sourceName, Harness: harness, SourceFile: sourceFile}, normalize.State{},
		func(e *schema.Event) error { events = append(events, *e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	got := roundTrip(t, events)
	for _, e := range got {
		e["event_version"] = "<event_version>"
	}
	return got
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
