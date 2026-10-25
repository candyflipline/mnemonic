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

// TestIndexLock pins the one exclusion every index writer passes through: the
// index dir's writer.lock. A writer holding it — what another process is,
// its own open file description — refuses HoldIndexLock and Ingest with the
// holder's pid named; an ingest of a process that holds the lock borrows it;
// Unlock frees the dir.
func TestIndexLock(t *testing.T) {
	store := filepath.Join(t.TempDir(), "index")
	root := t.TempDir()
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}

	// Another process holds the dir: its own open file description flocks the
	// lock file and leaves its pid in it, the way takeLock does.
	holder, err := os.OpenFile(filepath.Join(store, writerLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(holder, "pid %d, another-mnemonic\n", os.Getpid())

	// A second writer is refused, naming the holder's pid.
	_, err = HoldIndexLock(store)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("HoldIndexLock under a holder = %v, want ErrLocked", err)
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(err.Error(), want) {
		t.Errorf("HoldIndexLock under a holder = %q, want it to name the holder (%s)", err, want)
	}

	// So is an Ingest, before it lists or parses anything: no second writer
	// enters the log appends, the checkpoint renames, CURRENT.tmp or GC.
	if _, err := Ingest(root, store, time.Minute, true); !errors.Is(err, ErrLocked) {
		t.Fatalf("Ingest under a holder = %v, want ErrLocked", err)
	}
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	// With the dir free the first writer takes it, and its own ingests borrow
	// the lock instead of refusing themselves — serve and local hold the dir
	// for their whole life and still ingest.
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

	// Unlock frees the dir for the next writer.
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
