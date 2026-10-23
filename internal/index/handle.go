package index

import (
	"errors"
	"io/fs"
	"sync"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
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

// Ingest brings the store up to root's raw and serves the result.
func (ix *Index) Ingest(root string, opts IngestOptions) (IngestStats, error) {
	st, err := Ingest(root, ix.store, opts.Quiet, opts.SettleAll)
	return st, errors.Join(err, ix.Refresh())
}

// IngestPlain brings the store up to the machine's harness files read in
// place as KindPlain sources (local mode) and serves the result.
func (ix *Index) IngestPlain(globs []config.SourceGlob, author, machine string, opts IngestOptions) (IngestStats, error) {
	st, err := IngestPlain(ix.store, globs, author, machine, opts.Quiet, opts.SettleAll)
	return st, errors.Join(err, ix.Refresh())
}

// Refresh opens CURRENT when its bytes changed since the DB being served. A
// failed open keeps the old DB serving; the next Refresh retries.
func (ix *Index) Refresh() error {
	stamp, ok := CurrentStamp(ix.store)
	ix.mu.Lock()
	same := !ok || stamp == ix.cur.stamp
	ix.mu.Unlock()
	if same {
		return nil
	}
	db, err := OpenDB(ix.store)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ix.mu.Lock()
	old := ix.cur
	ix.cur = &snapshot{db: db, stamp: stamp}
	ix.retire(old)
	ix.mu.Unlock()
	return nil
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
type IngestOptions struct {
	Quiet     time.Duration
	SettleAll bool
}
