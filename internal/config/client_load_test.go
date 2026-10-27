package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testServerKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

func TestLoadClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cases := []struct {
		name, body, wantErr string
	}{
		{"valid", "server = \"quic://mnemonic.example:8741\"\nserver_key = \"" + testServerKey + "\"\nmachine = \"mbp\"\n", ""},
		{"https server", "server = \"https://mnemonic.example\"\nserver_key = \"" + testServerKey + "\"\n", `server "https://mnemonic.example" must be quic://host:port`},
		{"http server", "server = \"http://127.0.0.1:8740\"\nserver_key = \"" + testServerKey + "\"\n", `server "http://127.0.0.1:8740" must be quic://host:port`},
		{"missing server_key", "server = \"quic://h:8741\"\n", "server_key is required: run `mnemonic key --file <server key file>` on the server"},
		{"bad server_key", "server = \"quic://h:8741\"\nserver_key = \"nope\"\n", `server_key: key "nope" must be unpadded base64url of 32 bytes`},
		{"token", "server = \"quic://h:8741\"\ntoken = \"t\"\n", "token is gone: keys replaced tokens: run `mnemonic key` and set server_key"},
		{"token_file", "server = \"quic://h:8741\"\ntoken_file = \"~/t\"\n", "token_file is gone: keys replaced tokens: run `mnemonic key` and set server_key"},
		{"push_address", "server = \"quic://h:8741\"\npush_address = \"h:1\"\n", `push_address is gone: set server = "quic://host:port"`},
		{"unknown key", "server = \"quic://h:8741\"\nserver_ky = \"x\"\n", `unknown key "server_ky"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadClient(path)
			if c.wantErr != "" {
				want := "config: " + path + ": " + c.wantErr
				if err == nil || err.Error() != want {
					t.Fatalf("err = %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Server != "quic://mnemonic.example:8741" || cfg.Addr() != "mnemonic.example:8741" ||
				cfg.ServerKey != testServerKey || cfg.KeyFile != home+"/.mnemonic/key" || cfg.Machine != "mbp" ||
				!strings.HasPrefix(cfg.Sources[0].Glob, home) {
				t.Fatalf("cfg = %+v", cfg)
			}
		})
	}
}
