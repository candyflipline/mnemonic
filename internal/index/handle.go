package index

import (
	"errors"
	"sync"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/redact"
)

type Index struct {
	store string
	lock  *IndexLock
	mu    sync.Mutex
	cur   *snapshot
}

type snapshot struct {
	db      *DB
	stamp   uint64
	refs    int
	retired bool
}

func Open(store string) (*Index, error) {
	ix := &Index{store: store, cur: &snapshot{db: emptyDB()}}
	return ix, ix.Refresh()
}

func OpenExclusive(store string) (*Index, error) {
	lock, err := HoldIndexLock(store)
	if err != nil {
		return nil, err
	}
	ix, err := Open(store)
	if err != nil {
		return nil, errors.Join(err, lock.Unlock())
	}
	ix.lock = lock
	return ix, nil
}

func emptyDB() *DB { return &DB{sessPart: map[string]int{}} }

func (ix *Index) Ingest(root string, opts IngestOptions) (IngestStats, error) {
	st, err := Ingest(root, ix.store, opts.Quiet, opts.SettleAll)
	return st, errors.Join(err, ix.Refresh())
}

func (ix *Index) IngestPlain(globs []config.SourceGlob, author, machine string, red *redact.Redactor, opts IngestOptions) (IngestStats, error) {
	st, err := IngestPlain(ix.store, globs, author, machine, red, opts)
	return st, errors.Join(err, ix.Refresh())
}

// openTries bounds the CURRENT re-reads when a part CURRENT names does not
// open. The writer's GC removes only parts the newest CURRENT does not name,
// and it publishes that CURRENT before removing, so a re-read names live
// parts; a store that still fails is broken and the error must reach the
// caller rather than serve as an empty index.
const openTries = 3

func (ix *Index) Refresh() error { return ix.refresh(nil) }

func (ix *Index) refresh(opened map[string]*Part) error {
	stamp, ok := CurrentStamp(ix.store)
	ix.mu.Lock()
	same := !ok || stamp == ix.cur.stamp
	ix.mu.Unlock()
	if same {
		closeParts(opened)
		return nil
	}
	for try := 1; ; try++ {
		db, err := openDB(ix.store, ix.Snapshot, opened)
		opened = nil
		if err == nil {
			ix.mu.Lock()
			old := ix.cur
			ix.cur = &snapshot{db: db, stamp: stamp}
			ix.retireLocked(old)
			ix.mu.Unlock()
			return nil
		}
		if errors.As(err, new(noCurrentError)) {
			return nil
		}
		var missing *PartOpenError
		if !errors.As(err, &missing) || try == openTries {
			return err
		}
	}
}

func (ix *Index) Snapshot() (db *DB, release func()) {
	ix.mu.Lock()
	s := ix.cur
	s.refs++
	ix.mu.Unlock()
	var once sync.Once
	return s.db, func() {
		once.Do(func() {
			ix.mu.Lock()
			s.refs--
			if s.retired && s.refs == 0 {
				s.db.Close()
			}
			ix.mu.Unlock()
		})
	}
}

func (ix *Index) retireLocked(s *snapshot) {
	s.retired = true
	if s.refs == 0 {
		s.db.Close()
	}
}

func (ix *Index) Close() error {
	ix.mu.Lock()
	ix.retireLocked(ix.cur)
	ix.mu.Unlock()
	if ix.lock != nil {
		return ix.lock.Unlock()
	}
	return nil
}

type IngestOptions struct {
	Quiet     time.Duration
	SettleAll bool
}
