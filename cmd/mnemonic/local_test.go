//go:build server

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRefuseClientDir(t *testing.T) {
	home := t.TempDir()
	client := filepath.Join(home, ".mnemonic")
	if err := os.MkdirAll(filepath.Join(client, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(client, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(client, "sub"), filepath.Join(home, "sublink")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(home)
	darwin := runtime.GOOS == "darwin"

	cases := []struct {
		name, dir string
		refused   bool
	}{
		{"exact", client, true},
		{"trailing slash and dots", client + "/./", true},
		{"relative from home", ".mnemonic", true},
		{"case variant", filepath.Join(home, ".Mnemonic"), darwin},
		{"symlink to it", filepath.Join(home, "link"), true},
		{"symlink to a subdirectory", filepath.Join(home, "sublink"), true},
		{"existing subdirectory", filepath.Join(client, "sub"), true},
		{"subdirectory not yet created", filepath.Join(client, "a", "b"), true},
		{"through the symlink, not yet created", filepath.Join(home, "link", "a"), true},
		{"dotdot back into it", filepath.Join(home, "x", "..", ".mnemonic"), true},
		{"the default", filepath.Join(home, ".mnemonic-local"), false},
		{"a sibling sharing the prefix", filepath.Join(home, ".mnemonicx"), false},
		{"a fresh directory", filepath.Join(home, "elsewhere", "data"), false},
	}
	for _, c := range cases {
		err := refuseClientDir(c.dir, home)
		if (err != nil) != c.refused {
			t.Errorf("%s: refuseClientDir(%q) = %v, refused want %v", c.name, c.dir, err, c.refused)
		}
	}
}
