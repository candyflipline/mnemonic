package index

import (
	"errors"
	"io/fs"
	"sync"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/redact"
)

// Index is the engine handle the server, MCP and the harness hold: a store
// dir and the DB of the CURRENT it last opened. Snapshot pins that DB for one
// call; Refresh swaps in the DB of a newer CURRENT, and the old one is closed
// once the last call pinning it releases it.
type Index struct {
	store string
	mu    sync.Mutex
	cur   *snapshot
}

type snapshot struct {
	db      *DB
	stamp   uint64
	refs    int
	retired bool
}

// Open is the Index over store, serving its CURRENT, or nothing until the
// first ingest commits one.
func Open(store string) (*Index, error) {
	ix := &Index{store: store, cur: &snapshot{db: emptyDB()}}
	return ix, ix.Refresh()
}

func emptyDB() *DB { return &DB{sessPart: map[string]int{}} }

// openRetryHook, non-nil in tests, runs between open retries.
var openRetryHook func()

// Ingest brings the store up to root's raw and serves the result.
func (ix *Index) Ingest(root string, opts IngestOptions) (IngestStats, error) {
	st, err := Ingest(root, ix.store, opts.Quiet, opts.SettleAll)
	return st, errors.Join(err, ix.Refresh())
}

// IngestPlain brings the store up to the machine's harness files read in
// place as KindPlain sources (local mode) and serves the result.
func (ix *Index) IngestPlain(globs []config.SourceGlob, author, machine string, opts IngestOptions) (IngestStats, error) {
	st, err := IngestPlain(ix.store, globs, author, machine, opts)
	return st, errors.Join(err, ix.Refresh())
}

// openTries bounds the CURRENT re-reads when a part CURRENT names does not
// open. The writer's GC removes only parts the newest CURRENT does not name,
// and it publishes that CURRENT before removing, so a re-read names live
// parts; a store that still fails is broken and the error must reach the
// caller rather than serve as an empty index.
const openTries = 3

// Refresh opens CURRENT when its bytes changed since the DB being served,
// taking from that DB the parts it still names (openDB). No
// CURRENT is nothing published yet and keeps the DB being served (Open's is
// empty); a part CURRENT names that no longer opens is retried on a freshly
// read CURRENT a bounded number of times and, still failing, returns the
// error — never an empty DB with a nil error, which is what export would
// write out.
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
		prev, release := ix.Snapshot()
		db, err := openDB(ix.store, prev, opened)
		opened = nil
		release()
		if err == nil {
			ix.mu.Lock()
			old := ix.cur
			ix.cur = &snapshot{db: db, stamp: stamp}
			ix.retire(old)
			ix.mu.Unlock()
			return nil
		}
		if errors.Is(err, fs.ErrNotExist) { // no CURRENT: nothing published yet
			return nil
		}
		var missing *PartOpenError
		if !errors.As(err, &missing) || try == openTries {
			return err
		}
		if openRetryHook != nil {
			openRetryHook()
		}
	}
}

// Snapshot pins the DB being served; release unpins it.
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

// retire closes s now or when its last pin releases it; ix.mu is held.
func (ix *Index) retire(s *snapshot) {
	s.retired = true
	if s.refs == 0 {
		s.db.Close()
	}
}

func (ix *Index) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.retire(ix.cur)
	return nil
}

// IngestOptions is when a hot source settles into a cold part: once its
// manifest updated_ms is Quiet behind the newest one, or now with SettleAll.
// Red is the redactor local mode hands its ingest (every parsed line goes
// through it, as push's upload does); serve leaves it nil — its raw was
// redacted before it arrived.
type IngestOptions struct {
	Quiet     time.Duration
	SettleAll bool
	Red       *redact.Redactor
}
