package index

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// ErrLocked is why a writer was refused an index dir: another process holds
// its writer lock. The refusal's text names the holder's pid when the lock
// file says one.
var ErrLocked = errors.New("index dir locked by another writer")

const writerLockName = "writer.lock"

// IndexLock is this process's exclusive writer lock on one index dir, held
// for as long as it may write there: flock(2) LOCK_EX on writer.lock, taken
// without waiting. The lock conflicts across processes and across a process's
// own opens, so the registry below is what makes a process's own ingests work
// while it holds the dir (serve, local): they borrow the held lock instead of
// taking a second one.
type IndexLock struct {
	f          *os.File
	key        string
	ownerHolds bool
	ingests    int
}

type lockRegistry struct {
	mu    sync.Mutex
	locks map[string]*IndexLock
}

var registry = &lockRegistry{locks: map[string]*IndexLock{}}

// HoldIndexLock takes store's writer lock now and holds it until Unlock, so
// serve or local owns the dir for its whole life. It refuses, with the holder
// named, when another writer holds the dir — and answers with the lock this
// process already holds when it is called again for the same dir.
func HoldIndexLock(store string) (*IndexLock, error) {
	r := registry
	r.mu.Lock()
	defer r.mu.Unlock()
	l, err := r.get(store)
	if err != nil {
		return nil, err
	}
	l.ownerHolds = true
	return l, nil
}

// IngestLock is the writer lock every ingest passes through: it borrows the
// lock this process holds for the dir when it holds one, and otherwise takes
// the dir for this one ingest, refusing with the holder named while another
// process holds it. The release must be called when the ingest is done; the
// dir is freed when the last ingest on it is done and no HoldIndexLock owner
// still holds it.
func IngestLock(store string) (release func(), err error) {
	r := registry
	r.mu.Lock()
	defer r.mu.Unlock()
	l, err := r.get(store)
	if err != nil {
		return nil, err
	}
	l.ingests++
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		l.ingests--
		r.dropIfIdle(l)
	}, nil
}

// Unlock ends the HoldIndexLock owner's hold. The dir is freed at once, or
// when the ingest still running on it is done.
func (l *IndexLock) Unlock() error {
	r := registry
	r.mu.Lock()
	defer r.mu.Unlock()
	l.ownerHolds = false
	return r.dropIfIdle(l)
}

func (r *lockRegistry) get(store string) (*IndexLock, error) {
	key := filepath.Clean(store)
	if l := r.locks[key]; l != nil {
		return l, nil
	}
	l, err := takeLock(store)
	if err != nil {
		return nil, err
	}
	r.locks[key] = l
	return l, nil
}

func (r *lockRegistry) dropIfIdle(l *IndexLock) error {
	if l.ownerHolds || l.ingests > 0 || r.locks[l.key] != l {
		return nil
	}
	delete(r.locks, l.key)
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func takeLock(store string) (*IndexLock, error) {
	path := filepath.Join(store, writerLockName)
	if err := os.MkdirAll(store, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrLocked, readHolder(path))
		}
		return nil, err
	}
	stampHolder(f)
	return &IndexLock{f: f, key: filepath.Clean(store)}, nil
}

func stampHolder(f *os.File) {
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "pid %d, %s\n", os.Getpid(), filepath.Base(os.Args[0]))
}

func readHolder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "another writer"
	}
	line, _, _ := strings.Cut(string(b), "\n")
	if line = strings.TrimSpace(line); line != "" && len(line) <= 128 {
		return line
	}
	return "another writer"
}
