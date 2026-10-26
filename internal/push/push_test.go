package push

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchdPlistLints(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil is macOS-only")
	}
	out, err := exec.Command(plutil, "-lint", filepath.Join("..", "..", "ops", "launchd", "com.mnemonic.push.plist")).CombinedOutput()
	if err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("plutil -lint said %q, want OK", out)
	}
}
