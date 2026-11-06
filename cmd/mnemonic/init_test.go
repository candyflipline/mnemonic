package main

import (
	"strings"
	"testing"
)

func TestGuardTakeover(t *testing.T) {
	const own = "/Users/a/Library/LaunchAgents/com.mnemonic.push.plist"
	printed := func(path string) string {
		return "gui/501/com.mnemonic.push = {\n\tactive count = 1\n\tpath = " + path + "\n\ttype = LaunchAgent\n\tstate = running\n\n\tprogram = /bin/mnemonic\n\tstdout path = /Users/a/.mnemonic/push.log\n}\n"
	}
	for _, tc := range []struct {
		name    string
		out     string
		loaded  bool
		force   bool
		wantErr []string
	}{
		{"not loaded", "", false, false, nil},
		{"same plist", printed(own), true, false, nil},
		{"other plist", printed("/tmp/x/Library/LaunchAgents/com.mnemonic.push.plist"), true, false,
			[]string{"/tmp/x/Library/LaunchAgents/com.mnemonic.push.plist", "--force"}},
		{"other plist forced", printed("/tmp/x/Library/LaunchAgents/com.mnemonic.push.plist"), true, true, nil},
		{"loaded without path", "gui/501/com.mnemonic.push = {\n\tstate = running\n}\n", true, false, []string{"--force"}},
	} {
		err := guardTakeover("com.mnemonic.push", tc.out, tc.loaded, own, tc.force)
		if tc.wantErr == nil {
			if err != nil {
				t.Errorf("%s: got %v, want nil", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: got nil, want an error naming %v", tc.name, tc.wantErr)
			continue
		}
		for _, w := range tc.wantErr {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q lacks %q", tc.name, err, w)
			}
		}
	}
}

func TestBrewStablePath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/opt/homebrew/Cellar/mnemonic/0.8.0/bin/mnemonic", "/opt/homebrew/opt/mnemonic/bin/mnemonic"},
		{"/usr/local/Cellar/mnemonic/0.8.0/bin/mnemonic", "/usr/local/opt/mnemonic/bin/mnemonic"},
		{"/home/linuxbrew/.linuxbrew/Cellar/mnemonic/0.8.0_1/bin/mnemonic", "/home/linuxbrew/.linuxbrew/opt/mnemonic/bin/mnemonic"},
		{"/Users/a/.local/bin/mnemonic", "/Users/a/.local/bin/mnemonic"},
		{"/opt/homebrew/Cellar/mnemonic/0.8.0/libexec/mnemonic", "/opt/homebrew/Cellar/mnemonic/0.8.0/libexec/mnemonic"},
	} {
		if got := brewStablePath(tc.in); got != tc.want {
			t.Errorf("brewStablePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
