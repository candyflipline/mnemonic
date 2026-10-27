package redact

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuiltinRuleIDs(t *testing.T) {
	r := Default()
	want := []string{
		"aws-access-token", "github-token", "github-fine-grained-token",
		"gitlab-token", "slack-token", "slack-webhook", "google-api-key",
		"google-oauth-token", "pem-private-key", "openssh-private-key",
		"jwt", "stripe-token", "stripe-webhook-secret", "anthropic-api-key",
		"openai-api-key", "huggingface-token", "sendgrid-api-key",
		"discord-webhook", "npm-token", "pypi-token", "digitalocean-token",
		"databricks-token", "dockerhub-token", "linear-api-key",
		"notion-token", "generic-api-key",
	}
	got := make([]string, len(r.rules))
	for i := range r.rules {
		got[i] = r.rules[i].id
	}
	if len(got) != len(want) {
		t.Fatalf("%d rules, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rule %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLineTable(t *testing.T) {
	r := Default()
	cases := []struct {
		name, in, want string
	}{
		{
			name: "specific families",
			in:   `{"text":"aws=AKIAIOSFODNN7EXAMPLE ghp_abcdefghijklmnopqrstuvwxyzABCDE12345 glpat-abcdefABCDEF12345678 xoxb-123456789012-ABCDEFGHIJKL AIza0123456789abcdefghijklmnopqrstuvwxy sk_live_abcdefghijklmnop"}`,
			want: `{"text":"aws=[REDACTED:aws-access-token] [REDACTED:github-token] [REDACTED:gitlab-token] [REDACTED:slack-token] [REDACTED:google-api-key] [REDACTED:stripe-token]"}`,
		},
		{
			name: "service token prefixes",
			in:   `{"text":"sk-ant-AbCdEfGhIjKlMnOpQrStUvWxYz1234567890 sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz1234567890 hf_abcdefghijklmnopqrstuvwxyzABCDEFGH SG.ABCDEFGHIJKLMNOPQRSTUV.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz1234567890 npm_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ pypi-abcdefghijklmnopqrstuvwxyzABCDEFGHIJ dop_v1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef dapi0123456789abcdef0123456789abcdef dckr_pat_AbCdEfGhIjKlMnOpQrStUvWxYz1234567890 lin_api_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ secret_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"}`,
			want: `{"text":"[REDACTED:anthropic-api-key] [REDACTED:openai-api-key] [REDACTED:huggingface-token] [REDACTED:sendgrid-api-key] [REDACTED:slack-webhook] [REDACTED:discord-webhook] [REDACTED:npm-token] [REDACTED:pypi-token] [REDACTED:digitalocean-token] [REDACTED:databricks-token] [REDACTED:dockerhub-token] [REDACTED:linear-api-key] [REDACTED:notion-token]"}`,
		},
		{
			name: "pem and jwt across escaped newlines",
			in:   `{"text":"-----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY----- jwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature"}`,
			want: `{"text":"[REDACTED:pem-private-key] jwt=[REDACTED:jwt]"}`,
		},
		{
			name: "entropy gate",
			in:   `{"text":"password = \"passwordpasswordpassword\" token = \"Vb9pQx7Lm2Nf8Rs4Ty6Ua0Kc3Wd5Ez\""}`,
			want: `{"text":"password = \"passwordpasswordpassword\" token = \"[REDACTED:generic-api-key]\""}`,
		},
		{
			name: "idempotent scrub",
			in:   `{"text":"token = Vb9pQx7Lm2Nf8Rs4Ty6Ua0Kc3Wd5Ez"}`,
			want: `{"text":"token = [REDACTED:generic-api-key]"}`,
		},
		{
			name: "placeholder protected",
			in:   `{"text":"[REDACTED:github-token]"}`,
			want: `{"text":"[REDACTED:github-token]"}`,
		},
		{
			name: "batch raw_json",
			in:   `{"token":"ghp_abcdefghijklmnopqrstuvwxyzABCDE12345"}`,
			want: `{"token":"[REDACTED:github-token]"}`,
		},
		{
			name: "batch input_json as one escaped literal",
			in:   `{"text":"{\"api_key\":\"AaBbCcDdEeFfGgHhIiJjKkLlMmNnOoPpQqRrSsTtUuVv1234567890+/\"}"}`,
			want: `{"text":"{\"api_key\":\"[REDACTED:generic-api-key]\"}"}`,
		},
		{
			name: "non-json line",
			in:   `malformed ghp_abcdefghijklmnopqrstuvwxyzABCDE12345`,
			want: `malformed [REDACTED:github-token]`,
		},
		{
			name: "plain line unchanged",
			in:   `{"text":"hello world","n":3,"ok":true}`,
			want: `{"text":"hello world","n":3,"ok":true}`,
		},
		{
			name: "trailing newline preserved",
			in:   "{\"text\":\"hello\"}\n",
			want: "{\"text\":\"hello\"}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(tc.in)
			got := r.Line(in)
			if !bytes.Equal(got, []byte(tc.want)) {
				t.Fatalf("Line()\n got %q\nwant %q", got, tc.want)
			}
			if json.Valid(in) && !json.Valid(got) {
				t.Fatalf("Line broke JSON: %q", got)
			}
			if again := r.Line(got); !bytes.Equal(again, got) {
				t.Fatalf("Line not a fixpoint: %q -> %q", got, again)
			}
		})
	}
}

func TestExtraPattern(t *testing.T) {
	r, err := New([]string{`acme_internal_[0-9A-Za-z]{32}`})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	in := []byte(`{"text":"token=acme_internal_0123456789abcdef0123456789abcdef"}`)
	want := `{"text":"token=[REDACTED:extra-pattern-1]"}`
	got := r.Line(in)
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("Line()\n got %q\nwant %q", got, want)
	}
	if again := r.Line(got); !bytes.Equal(again, got) {
		t.Fatalf("Line not a fixpoint: %q -> %q", got, again)
	}
}

func TestNewInvalidPattern(t *testing.T) {
	_, err := New([]string{"("})
	if err == nil {
		t.Fatal("New accepted an invalid pattern")
	}
	if !strings.Contains(err.Error(), "extra_patterns[0]") {
		t.Fatalf("error %q does not name extra_patterns[0]", err)
	}
}

func TestLargeSecretFreeText(t *testing.T) {
	r := Default()
	in := []byte(`{"text":"` + strings.Repeat("z", 2*1024*1024) + `"}`)
	got := r.Line(in)
	if !bytes.Equal(got, in) {
		t.Fatalf("2 MiB secret-free line changed: %d bytes vs %d", len(got), len(in))
	}
}
