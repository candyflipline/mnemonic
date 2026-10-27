package index

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIndexLock(t *testing.T) {
	store := filepath.Join(t.TempDir(), "index")
	root := t.TempDir()
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}

	holder, err := os.OpenFile(filepath.Join(store, writerLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(holder, "pid %d, another-mnemonic\n", os.Getpid())

	_, err = HoldIndexLock(store)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("HoldIndexLock under a holder = %v, want ErrLocked", err)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(err.Error(), want) {
		t.Errorf("HoldIndexLock under a holder = %q, want it to name the holder (%s)", err, want)
	}

	if _, err := Ingest(root, store, time.Minute, true); !errors.Is(err, ErrLocked) {
		t.Fatalf("Ingest under a holder = %v, want ErrLocked", err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	first, err := HoldIndexLock(store)
	if err != nil {
		t.Fatalf("HoldIndexLock on a free dir: %v", err)
	}
	if _, err := Ingest(root, store, time.Minute, true); err != nil {
		t.Fatalf("Ingest under the process's own lock: %v", err)
	}
	if _, ok := CurrentStamp(store); !ok {
		t.Error("ingest under the held lock wrote no CURRENT")
	}

	if err := first.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	second, err := HoldIndexLock(store)
	if err != nil {
		t.Fatalf("HoldIndexLock after Unlock: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Fatalf("second Unlock: %v", err)
	}
}
