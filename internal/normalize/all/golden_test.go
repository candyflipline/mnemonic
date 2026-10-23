package all_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/normalize"
	_ "github.com/candyflipline/mnemonic/internal/normalize/all"
	"github.com/candyflipline/mnemonic/internal/repo"
	"github.com/candyflipline/mnemonic/internal/schema"
)

var cases = []struct {
	name, harness, sourceName, fixture, sourceFile string
	overlays                                       string
}{
	{"codex", "codex", "golden-codex", "codex/session.jsonl", "/fixtures/codex/session.jsonl", ""},
	{"claude_code", "claude-code", "golden-claude-code", "claude-code/session.jsonl", "/fixtures/claude-code/session.jsonl", ""},
	{"kimi_cli", "kimi-cli", "golden-kimi-cli", "kimi-cli/wire.jsonl", "/fixtures/kimi-cli/wire.jsonl", ""},
	{"qwen_code", "qwen-code", "golden-qwen-code", "qwen-code/session.jsonl", "/fixtures/qwen-code/session.jsonl", ""},
	{"kiro_cli", "kiro-cli", "golden-kiro-cli", "kiro/session.jsonl", "/fixtures/kiro/11111111-2222-4333-8444-555555555555.jsonl", ""},
	{"opencode", "opencode", "golden-opencode", "opencode/session.jsonl", "/fixtures/opencode/session.jsonl", ""},
	{"cursor", "cursor", "golden-cursor",
		"cursor/projects/demo/agent-transcripts/11111111-2222-4333-8444-555555555555/11111111-2222-4333-8444-555555555555.jsonl",
		"/fixtures/cursor/projects/demo/agent-transcripts/11111111-2222-4333-8444-555555555555/11111111-2222-4333-8444-555555555555.jsonl", ""},
	{"hermes_trajectory", "hermes", "golden-hermes-trajectory", "hermes/trajectories/001.jsonl", "/fixtures/hermes/trajectories/001.jsonl", ""},
	{"pi_coding_agent", "pi-coding-agent", "golden-pi", "pi/session.jsonl", "/fixtures/pi/session.jsonl", ""},
	{"prime_agent", "prime-agent", "golden-prime-agent", "prime-agent/session.jsonl", "/fixtures/prime-agent/session.jsonl", ""},
	{"claude_code_roots", "claude-code", "golden-claude-code", "claude-code/session.jsonl", "/fixtures/claude-code/session.jsonl", "claude_code"},
}

func TestGoldens(t *testing.T) {
	ran := 0
	for _, c := range cases {
		if _, ok := normalize.For(c.harness); !ok {
			continue
		}
		ran++
		t.Run(c.name, func(t *testing.T) {
			got := parse(t, c.harness, c.sourceName, c.fixture, c.sourceFile, c.overlays != "")

			var golden struct {
				Records []struct {
					EventRows []map[string]any `json:"event_rows"`
				} `json:"records"`
			}
			base := c.name
			if c.overlays != "" {
				base = c.overlays
			}
			raw, err := os.ReadFile(filepath.Join("../../../testdata/goldens", base+".json"))
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
			if c.overlays != "" {
				raw, err := os.ReadFile(filepath.Join("../../../testdata/goldens", c.name+".overlay.json"))
				if err != nil {
					t.Fatal(err)
				}
				var overlay []map[string]any
				if err := json.Unmarshal(raw, &overlay); err != nil {
					t.Fatal(err)
				}
				if len(overlay) != len(want) {
					t.Fatalf("overlay has %d rows, golden %s has %d", len(overlay), base, len(want))
				}
				for i, o := range overlay {
					for k, v := range o {
						if k != "moraine_tool_io" {
							want[i][k] = v
							continue
						}
						payload := decodeObject(t, want[i]["payload_json"])
						tio, ok := payload[k].(map[string]any)
						if !ok {
							t.Fatalf("overlay row %d: golden %s payload has no %s", i, base, k)
						}
						for f, fv := range v.(map[string]any) {
							tio[f] = fv
						}
						want[i]["payload_json"] = payload
						if i < len(got) {
							got[i]["payload_json"] = decodeObject(t, got[i]["payload_json"])
						}
					}
				}
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

var outputs = map[int]string{
	1: "86abac583cac7670c73428bd75331551c46e1459292091a644158c1ac5bf6c77",
	2: "c08d0d222d2142d7ab65401cddf0c2a92d2836f94bb9c39eb969b8dc73d84166",
}

func TestVersionNamesOutput(t *testing.T) {
	h := sha256.New()
	for _, c := range cases {
		if _, ok := normalize.For(c.harness); !ok {
			continue
		}
		b, err := json.Marshal(parse(t, c.harness, c.sourceName, c.fixture, c.sourceFile, c.overlays != ""))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s\n%s\n", c.name, b)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	switch want, ok := outputs[normalize.Version]; {
	case !ok:
		t.Fatalf("add %d: %q to outputs", normalize.Version, sum)
	case want != sum:
		t.Fatalf("parser output changed without normalize.Version: bump it to %d and add %d: %q to outputs; never edit an entry",
			normalize.Version+1, normalize.Version+1, sum)
	}
}

func parse(t *testing.T, harness, sourceName, fixture, sourceFile string, withRoots bool) []map[string]any {
	t.Helper()
	p, _ := normalize.For(harness)
	path := filepath.Join("../../../testdata/fixtures", fixture)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var book repo.Book
	if withRoots {
		raw, err := os.ReadFile(strings.TrimSuffix(path, filepath.Ext(path)) + ".roots")
		if err != nil {
			t.Fatal(err)
		}
		if book, err = repo.DecodeBook(raw); err != nil {
			t.Fatal(err)
		}
	}
	var events []schema.Event
	_, err = p.Parse(f, normalize.SourceMeta{SourceName: sourceName, Harness: harness, SourceFile: sourceFile, Roots: book}, normalize.State{},
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

func decodeObject(t *testing.T, v any) map[string]any {
	t.Helper()
	s, _ := v.(string)
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
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
