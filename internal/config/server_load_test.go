package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validServerToml = `data_dir = "/var/lib/mnemonic"
listen = "127.0.0.1:8740"

[[tokens]]
author = "chris"
sha256 = "6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b"
`

func writeServerToml(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadServer(t *testing.T) {
	t.Run("valid file", func(t *testing.T) {
		cfg, err := LoadServer(writeServerToml(t, validServerToml))
		if err != nil {
			t.Fatal(err)
		}
		want := Server{
			DataDir: "/var/lib/mnemonic",
			Listen:  "127.0.0.1:8740",
			Tokens: []TokenEntry{{
				Author: "chris",
				SHA256: "6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b",
			}},
		}
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("cfg = %+v, want %+v", cfg, want)
		}
	})

	for name, body := range map[string]string{
		"empty data_dir": strings.Replace(validServerToml, `/var/lib/mnemonic`, ``, 1),
		"empty listen":   strings.Replace(validServerToml, "127.0.0.1:8740", `""`, 1),
		"no tokens":      "data_dir = \"/tmp\"\nlisten = \"127.0.0.1:8740\"\n",
		"empty author": strings.Replace(validServerToml,
			`author = "chris"`, `author = ""`, 1),
		"short sha256": strings.Replace(validServerToml,
			"6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b", "abc123", 1),
		"uppercase sha256": strings.Replace(validServerToml,
			"6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b",
			"6B86B273FF34FCE19D6B804EFF5A3F5747ADA4EAA22F1D49C01E52DDB7875B4B", 1),
		"duplicate hash": validServerToml + `
[[tokens]]
author = "sam"
sha256 = "6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b"
`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := LoadServer(writeServerToml(t, body)); err == nil {
				t.Errorf("LoadServer accepted %s:\n%s", name, body)
			}
		})
	}
}
