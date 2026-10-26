package server

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/layout"
)

func TestRunEndsWhenBootListingFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	dir := t.TempDir()
	harness := filepath.Join(dir, "events", "author=a", "machine="+layout.ArchiveMachine, "harness=h")
	if err := os.MkdirAll(harness, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(harness, "ch-1.parquet"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(harness, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(harness, 0o700) })
	srv, err := New(dir, auth.NewTokens(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan error, 1)
	go func() { ran <- srv.Run(ctx) }()
	select {
	case err := <-ran:
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("Run = %v, want a permission error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept looping after the boot listing failed; the server stays up with push unserved")
	}
}
