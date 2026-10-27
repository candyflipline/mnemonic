package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/candyflipline/mnemonic/internal/auth"
)

var (
	keyA = strings.Repeat("A", 43)
	keyB = strings.Repeat("g", 43)
)

const validServerToml = `data_dir = "/var/lib/mnemonic"

[quic]
listen = "0.0.0.0:8741"

[[keys]]
key = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
author = "chris"
tenant = "acme"
scopes = ["read"]
`

func TestLoadServer(t *testing.T) {
	cases := []struct {
		name, toml, err string
	}{
		{"valid", validServerToml, ""},
		{"key entry with no tenant and no scopes", strings.Replace(validServerToml, "tenant = \"acme\"\nscopes = [\"read\"]\n", "", 1), ""},
		{"missing quic listen", strings.Replace(validServerToml, `listen = "0.0.0.0:8741"`, "", 1), "CFG: [quic] listen must be set"},
		{"bad quic port", strings.Replace(validServerToml, "0.0.0.0:8741", "0.0.0.0:0", 1), "CFG: [quic] listen must be a host:port with port 1..65535"},
		{"no keys", "data_dir = \"/tmp\"\n[quic]\nlisten = \":8741\"\n", "CFG: at least one [[keys]] entry is required"},
		{"bad key text", strings.Replace(validServerToml, keyA, "abc", 1), `CFG: keys[0] (chris): key "abc" must be unpadded base64url of 32 bytes`},
		{"duplicate key", validServerToml + "\n[[keys]]\nkey = \"" + keyA + "\"\nauthor = \"sam\"\ntenant = \"acme\"\n", "CFG: keys[1]: key already used by chris"},
		{"author in two tenants", validServerToml + "\n[[keys]]\nkey = \"" + keyB + "\"\nauthor = \"chris\"\n", "CFG: keys[1]: author chris is in tenant acme and default; an author sits in one tenant"},
		{"empty scopes", strings.Replace(validServerToml, `["read"]`, `[]`, 1), "CFG: keys[0] (chris): scopes is empty; omit it for the default"},
		{"unknown scope", strings.Replace(validServerToml, `["read"]`, `["write"]`, 1), `CFG: keys[0] (chris): unknown scope "write"; scopes are [read push annotate admin]`},
		{"old listen", "listen = \":443\"\n" + validServerToml, "CFG: listen is gone: the one UDP endpoint is [quic] listen"},
		{"old push_listen", "push_listen = \":8741\"\n" + validServerToml, "CFG: push_listen is gone: the one UDP endpoint is [quic] listen"},
		{"old tls", validServerToml + "\n[tls]\nhosts = [\"a.example\"]\n", "CFG: [tls] is gone: QUIC pins the server key, so there is no certificate; set [quic] listen and key_file"},
		{"old tokens", validServerToml + "\n[[tokens]]\nauthor = \"sam\"\nsha256 = \"00\"\n", "CFG: [[tokens]] is gone: admit a client key with [[keys]] (key, author, tenant, scopes); mnemonic key prints one"},
		{"unknown key", "colour = 1\n" + validServerToml, "CFG: unknown key colour"},
	}
	granted := map[string]struct {
		tenant string
		push   bool
	}{"valid": {"acme", false}, "key entry with no tenant and no scopes": {"default", true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.toml")
			if err := os.WriteFile(path, []byte(tc.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadServer(path)
			if tc.err != "" {
				if err == nil || strings.ReplaceAll(err.Error(), path, "CFG") != tc.err {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.QUIC.KeyFile != "/var/lib/mnemonic/server.key" {
				t.Errorf("key_file = %q", cfg.QUIC.KeyFile)
			}
			id, ok := cfg.Identities().Identify(keyA)
			want := granted[tc.name]
			if !ok || id.Author() != "chris" || id.Tenant() != want.tenant || id.Can(auth.Push) != want.push || !id.Can(auth.Read) {
				t.Errorf("Identify = %+v, %v", id, ok)
			}
		})
	}
}
