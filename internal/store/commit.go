package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/wire"
)

const (
	maxBatchAppends = 512
	maxBatchBytes   = 128 << 20
	queueBytes      = 256 << 20
	queueRequests   = 2 * maxBatchAppends
)

// Lines is a non-empty run of whole lines, each ending in '\n'.
type Lines struct{ b []byte }

var errLines = errors.New("body must be non-empty whole lines")

func ParseLines(b []byte) (Lines, error) {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return Lines{}, errLines
	}
	return Lines{b}, nil
}

// ErrReserved is NewClientSource's answer to a machine or harness only the
// server writes (HTTP 403, gRPC PermissionDenied).
var ErrReserved = errors.New("machine or harness is reserved for server-owned sources")

var errSegment = errors.New("bad path segment")

// ClientSource is the key of a client-owned source.
type ClientSource struct{ key layout.Key }

func NewClientSource(author, machine, harness, srcID string) (ClientSource, error) {
	for _, seg := range [...]string{author, machine, harness, srcID} {
		if !wire.ValidSegment(seg) {
			return ClientSource{}, errSegment
		}
	}
	if machine == ServerMachine || isServerHarness(harness) {
		return ClientSource{}, ErrReserved
	}
	return ClientSource{layout.Key{Author: author, Machine: machine, Harness: harness, SrcID: srcID}}, nil
}

// Op is one change to a source: Replace, Extend, Backfill, or the server's
// own append.
type Op interface{ isOp() }

type Replace struct {
	HeadHash, SourceName, Path string
	RawEnd                     int64
	Lines                      Lines
}

type Extend struct {
	Offset   int64
	HeadHash string
	RawEnd   int64
	Lines    Lines
}

// Backfill records RawEnd when the source is Offset long with HeadHash; Size,
// ZstSize and UpdatedMs stay, so the index re-parses nothing.
type Backfill struct {
	Offset   int64
	HeadHash string
	RawEnd   int64
}

type serverAppend struct {
	src        ServerSource
	requestKey string
	lines      Lines
}

func (Replace) isOp()      {}
func (Extend) isOp()       {}
func (Backfill) isOp()     {}
func (serverAppend) isOp() {}

// ParseOp maps a PUT's or an Append's fields onto an Op. Its errors read as
// the HTTP 400 bodies the PUT answered with.
func ParseOp(offset int64, headHash, sourceName, path string, rawEnd int64, body []byte) (Op, error) {
	if offset < 0 {
		return nil, errors.New("bad offset")
	}
	if headHash == "" || sourceName == "" || path == "" {
		return nil, errors.New("missing query parameter")
	}
	if rawEnd < 0 {
		return nil, errors.New("bad raw_end")
	}
	if len(body) == 0 && offset > 0 {
		return Backfill{Offset: offset, HeadHash: headHash, RawEnd: rawEnd}, nil
	}
	l, err := ParseLines(body)
	if err != nil {
		return nil, err
	}
	if offset == 0 {
		return Replace{HeadHash: headHash, SourceName: sourceName, Path: path, RawEnd: rawEnd, Lines: l}, nil
	}
	return Extend{Offset: offset, HeadHash: headHash, RawEnd: rawEnd, Lines: l}, nil
}

type Verdict uint8

const (
	// Applied: State is the source after the change.
	Applied Verdict = iota + 1
	// Conflict: nothing written; State is the source as stored, zero when
	// there is none.
	Conflict
	// Duplicate: a server append whose request key was already applied.
	Duplicate
)

// Ack is a request's answer once its batch is durable. The zero Ack is
// invalid; committed.acks makes every other one.
type Ack struct {
	verdict Verdict
	state   wire.Source
	batch   uint64
}

func (a Ack) Valid() bool        { return a.batch != 0 }
func (a Ack) Verdict() Verdict   { return a.verdict }
func (a Ack) State() wire.Source { return a.state }

type Ticket struct {
	done chan struct{}
	ack  Ack
	err  error
}

func newTicket() *Ticket { return &Ticket{done: make(chan struct{})} }

func (t *Ticket) resolve(a Ack) { t.ack = a; close(t.done) }

func (t *Ticket) fail(err error) { t.err = err; close(t.done) }

// Wait returns the Ack once the request's batch passed its last barrier, or
// the error that stopped the committer.
func (t *Ticket) Wait(ctx context.Context) (a Ack, err error) {
	select {
	case <-t.done:
		return t.ack, t.err
	case <-ctx.Done():
		return a, ctx.Err()
	}
}

// Committer is the one writer of raw/. Every client change and server append
// is a request on its queue; one goroutine commits them in batches. The table
// is every source's manifest, loaded at Open and written through.
type Committer struct {
	dataDir string
	barrier barrier
	closeFS func() error
	exit    func(error)

	reqs   chan *request
	budget budget

	intake sync.RWMutex
	closed bool

	mu    sync.RWMutex
	table map[layout.Key]*manifestBody

	changes changes
	seq     uint64
	stopped chan struct{}
	done    chan struct{}
	err     error
}

type request struct {
	key    layout.Key
	op     Op
	frame  []byte
	ticket *Ticket
}

// Open loads the table from every manifest under dataDir, removes the temp
// files a crash left, and starts the committer. A failed write or barrier
// stops the committer and exits the process.
func Open(dataDir string) (*Committer, error) {
	b, closeFS, err := openBarrier(dataDir)
	if err != nil {
		return nil, err
	}
	c, err := open(dataDir, b, func(err error) {
		log.Printf("store: committer stopped: %v", err)
		os.Exit(1)
	})
	if err != nil {
		closeFS()
		return nil, err
	}
	c.closeFS = closeFS
	return c, nil
}

func open(dataDir string, b barrier, exit func(error)) (*Committer, error) {
	table, loaded, err := loadTable(dataDir)
	if err != nil {
		return nil, err
	}
	if err := b.run(loaded); err != nil {
		return nil, err
	}
	c := &Committer{
		dataDir: dataDir,
		barrier: b,
		closeFS: func() error { return nil },
		exit:    exit,
		reqs:    make(chan *request, queueRequests),
		budget:  newBudget(queueBytes),
		table:   table,
		changes: changes{keys: map[layout.Key]struct{}{}, ready: make(chan struct{}, 1)},
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go c.run()
	return c, nil
}

// loadTable reads every manifest into the table, removes temp files, and
// returns what it read as touched: a manifest renamed before a crash may not
// be durable yet, and Open makes it so before any Welcome reports it.
func loadTable(dataDir string) (map[layout.Key]*manifestBody, touched, error) {
	table := map[layout.Key]*manifestBody{}
	var loaded touched
	root := filepath.Join(dataDir, "raw")
	authors, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return table, loaded, nil
	}
	if err != nil {
		return nil, loaded, err
	}
	for _, a := range authors {
		author, ok := strings.CutPrefix(a.Name(), "author=")
		if !ok || !a.IsDir() {
			continue
		}
		machines, err := os.ReadDir(filepath.Join(root, a.Name()))
		if err != nil {
			return nil, loaded, err
		}
		for _, m := range machines {
			machine, ok := strings.CutPrefix(m.Name(), "machine=")
			if !ok || !m.IsDir() {
				continue
			}
			harnesses, err := os.ReadDir(filepath.Join(root, a.Name(), m.Name()))
			if err != nil {
				return nil, loaded, err
			}
			for _, h := range harnesses {
				harness, ok := strings.CutPrefix(h.Name(), "harness=")
				if !ok || !h.IsDir() {
					continue
				}
				dir := filepath.Join(root, a.Name(), m.Name(), h.Name())
				if err := loadDir(table, &loaded, dir, layout.Key{Author: author, Machine: machine, Harness: harness}); err != nil {
					return nil, loaded, err
				}
			}
		}
	}
	return table, loaded, nil
}

func loadDir(table map[layout.Key]*manifestBody, loaded *touched, dir string, k layout.Key) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		name := f.Name()
		if isTemp(name) {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		srcID, ok := strings.CutSuffix(name, ".json")
		if !ok || strings.HasPrefix(name, ".") {
			continue
		}
		m, err := readManifest(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("store: manifest %s: %w", filepath.Join(dir, name), err)
		}
		k.SrcID = srcID
		table[k] = m
		loaded.files = append(loaded.files, filepath.Join(dir, name))
	}
	loaded.dirs = append(loaded.dirs, dir)
	return nil
}

func isTemp(name string) bool {
	return strings.HasPrefix(name, ".") &&
		(strings.Contains(name, ".json.") || strings.Contains(name, ".jsonl.zst."))
}

// Close stops intake, commits what is queued, and returns the error that
// stopped the committer, if any.
func (c *Committer) Close() error {
	c.intake.Lock()
	if !c.closed {
		c.closed = true
		close(c.reqs)
	}
	c.intake.Unlock()
	<-c.done
	if err := c.closeFS(); c.err == nil {
		return err
	}
	return c.err
}

var errClosed = errors.New("store: closed")

// Submit queues op for src and returns its ticket. The frame is encoded on the
// caller's goroutine; Submit blocks while queueBytes of frames are queued.
func (c *Committer) Submit(ctx context.Context, src ClientSource, op Op) (*Ticket, error) {
	var frame []byte
	switch o := op.(type) {
	case Replace:
		frame = encodeFrame(o.Lines)
	case Extend:
		frame = encodeFrame(o.Lines)
	case Backfill:
	default:
		return nil, fmt.Errorf("store: op %T is not a client op", op)
	}
	return c.submit(ctx, src.key, op, frame)
}

func (c *Committer) submit(ctx context.Context, k layout.Key, op Op, frame []byte) (*Ticket, error) {
	n := min(int64(len(frame)), queueBytes)
	if err := c.budget.acquire(ctx, n, c.stopped); err != nil {
		if c.stoppedErr() != nil {
			return nil, c.err
		}
		return nil, err
	}
	r := &request{key: k, op: op, frame: frame, ticket: newTicket()}
	c.intake.RLock()
	defer c.intake.RUnlock()
	if c.closed {
		c.budget.release(n)
		return nil, errClosed
	}
	if err := c.stoppedErr(); err != nil {
		c.budget.release(n)
		return nil, err
	}
	select {
	case c.reqs <- r:
		return r.ticket, nil
	case <-c.stopped:
		c.budget.release(n)
		return nil, c.err
	case <-ctx.Done():
		c.budget.release(n)
		return nil, ctx.Err()
	}
}

func (c *Committer) stoppedErr() error {
	select {
	case <-c.stopped:
		return c.err
	default:
		return nil
	}
}

// Append is the server-owned write: lines onto src, created when missing. A
// non-empty requestKey already applied to src or to the day before it changes
// nothing and reports duplicate; an empty one never dedups.
func (c *Committer) Append(src ServerSource, requestKey string, lines []byte) (_ wire.Source, duplicate bool, _ error) {
	if err := src.validate(); err != nil {
		return wire.Source{}, false, err
	}
	l, err := ParseLines(lines)
	if err != nil {
		return wire.Source{}, false, fmt.Errorf("store: %w", err)
	}
	ctx := context.Background()
	t, err := c.submit(ctx, src.key(), serverAppend{src: src, requestKey: requestKey, lines: l}, encodeFrame(l))
	if err != nil {
		return wire.Source{}, false, err
	}
	a, err := t.Wait(ctx)
	if err != nil {
		return wire.Source{}, false, err
	}
	return a.State(), a.Verdict() == Duplicate, nil
}

// Sources is author's sources on machine as the table holds them, sorted by
// SrcID.
func (c *Committer) Sources(author, machine string) []wire.Source {
	c.mu.RLock()
	out := []wire.Source{}
	for k, m := range c.table {
		if k.Author == author && k.Machine == machine {
			out = append(out, m.Source)
		}
	}
	c.mu.RUnlock()
	slices.SortFunc(out, func(a, b wire.Source) int { return strings.Compare(a.SrcID, b.SrcID) })
	return out
}

// Changed fires after a batch that changed a source commits.
func (c *Committer) Changed() <-chan struct{} { return c.changes.ready }

// TakeChanged returns every key changed since the last take and clears the
// set.
func (c *Committer) TakeChanged() []layout.Key {
	c.changes.mu.Lock()
	keys := c.changes.keys
	c.changes.keys = map[layout.Key]struct{}{}
	c.changes.mu.Unlock()
	out := make([]layout.Key, 0, len(keys))
	for k := range keys {
		out = append(out, k)
	}
	slices.SortFunc(out, compareKeys)
	return out
}

func compareKeys(a, b layout.Key) int {
	for _, p := range [...][2]string{{a.Author, b.Author}, {a.Machine, b.Machine}, {a.Harness, b.Harness}, {a.SrcID, b.SrcID}} {
		if c := strings.Compare(p[0], p[1]); c != 0 {
			return c
		}
	}
	return 0
}

type changes struct {
	mu    sync.Mutex
	keys  map[layout.Key]struct{}
	ready chan struct{}
}

func (ch *changes) add(keys []layout.Key) {
	if len(keys) == 0 {
		return
	}
	ch.mu.Lock()
	for _, k := range keys {
		ch.keys[k] = struct{}{}
	}
	ch.mu.Unlock()
	select {
	case ch.ready <- struct{}{}:
	default:
	}
}

func (c *Committer) run() {
	defer close(c.done)
	var held []*request
	for {
		if len(held) == 0 {
			r, ok := <-c.reqs
			if !ok {
				return
			}
			held = append(held, r)
		}
		held = c.fill(held)
		n, err := c.commit(batchOf(held))
		if err != nil {
			c.stop(err, held)
			return
		}
		held = held[n:]
	}
}

// fill takes every request already queued, without waiting, until the batch
// caps are reached.
func (c *Committer) fill(held []*request) []*request {
	size := 0
	for _, r := range held {
		size += len(r.frame)
	}
	for len(held) < maxBatchAppends && size <= maxBatchBytes {
		select {
		case r, ok := <-c.reqs:
			if !ok {
				return held
			}
			held = append(held, r)
			size += len(r.frame)
		default:
			return held
		}
	}
	return held
}

func batchOf(held []*request) []*request {
	n, size := 1, len(held[0].frame)
	for n < len(held) && n < maxBatchAppends && size+len(held[n].frame) <= maxBatchBytes {
		size += len(held[n].frame)
		n++
	}
	return held[:n]
}

// commit commits the prefix of reqs plan admits and returns its length.
func (c *Committer) commit(reqs []*request) (int, error) {
	p := plan(c.table, reqs, func() int64 { return time.Now().UnixMilli() })
	w, err := p.write(c.dataDir)
	if err != nil {
		if _, ok := err.(*writeSpaceError); !ok {
			return 0, err
		}
		if cleanupErr := w.rollback(); cleanupErr != nil {
			return 0, fmt.Errorf("store: W1 rollback: %w", errors.Join(err, cleanupErr))
		}
		// A rewrite already unlinked its manifest. Match crash row 3 rather
		// than trying to restore it with another write on a full disk.
		c.mu.Lock()
		for _, key := range w.unlinked {
			delete(c.table, key)
		}
		c.mu.Unlock()
		for _, req := range p.reqs {
			req.ticket.fail(err)
		}
		c.budget.release(int64(size(p.reqs)))
		return len(p.reqs), nil
	}
	d, err := w.sync(c.barrier)
	if err != nil {
		return 0, err
	}
	r, err := d.linkRaws(c.barrier)
	if err != nil {
		return 0, err
	}
	k, err := r.linkManifests(c.barrier, c.seq+1)
	if err != nil {
		return 0, err
	}
	c.seq++
	c.mu.Lock()
	for _, sp := range p.srcs {
		after := sp.after
		c.table[sp.key] = &after
	}
	c.mu.Unlock()
	c.changes.add(k.changed)
	for i, a := range k.acks() {
		reqs[i].ticket.resolve(a)
	}
	c.budget.release(int64(size(p.reqs)))
	return len(p.reqs), nil
}

func size(reqs []*request) int {
	n := 0
	for _, r := range reqs {
		n += min(len(r.frame), queueBytes)
	}
	return n
}

func (c *Committer) stop(err error, held []*request) {
	c.err = err
	close(c.stopped)
	for _, r := range held {
		r.ticket.fail(err)
	}
	c.exit(err)
	for r := range c.reqs {
		r.ticket.fail(err)
	}
}

// planned is the batch decided in memory: per request its verdict and the
// source state it answers with, per touched source its after manifest and
// frames. A Replace of a source already planned ends the batch before it, so
// no acked frame is overwritten inside one batch.
type planned struct {
	reqs    []*request
	verdict []Verdict
	state   []wire.Source
	srcs    []*sourcePlan
}

// sourcePlan is one source's change. A rewrite (Replace, or a server
// source's create) writes a new raw from frames and unlinks the old manifest
// first; otherwise the raw is truncated to before.ZstSize and frames
// appended. Requests on one source fold in queue order.
type sourcePlan struct {
	key     layout.Key
	before  *manifestBody
	after   manifestBody
	rewrite bool
	frames  [][]byte
}

func plan(table map[layout.Key]*manifestBody, reqs []*request, now func() int64) planned {
	p := planned{reqs: reqs, verdict: make([]Verdict, len(reqs)), state: make([]wire.Source, len(reqs))}
	bySrc := map[layout.Key]*sourcePlan{}
	cur := func(k layout.Key) *manifestBody {
		if sp := bySrc[k]; sp != nil {
			return &sp.after
		}
		return table[k]
	}
	touch := func(k layout.Key) *sourcePlan {
		if sp := bySrc[k]; sp != nil {
			return sp
		}
		sp := &sourcePlan{key: k, before: table[k]}
		if sp.before != nil {
			sp.after = *sp.before
			sp.after.Applied = slices.Clone(sp.before.Applied)
		}
		bySrc[k] = sp
		p.srcs = append(p.srcs, sp)
		return sp
	}
	for i, r := range reqs {
		m := cur(r.key)
		switch op := r.op.(type) {
		case Replace:
			if bySrc[r.key] != nil {
				p.reqs, p.verdict, p.state = reqs[:i], p.verdict[:i], p.state[:i]
				return p
			}
			var prevMs int64
			if m != nil {
				prevMs = m.UpdatedMs
			}
			sp := touch(r.key)
			sp.rewrite = true
			sp.frames = [][]byte{r.frame}
			sp.after = manifestBody{
				Source: wire.Source{
					SrcID:      r.key.SrcID,
					Harness:    r.key.Harness,
					SourceName: op.SourceName,
					Path:       op.Path,
					Size:       int64(len(op.Lines.b)),
					RawEnd:     op.RawEnd,
					HeadHash:   op.HeadHash,
					UpdatedMs:  max(now(), prevMs+1),
				},
				ZstSize: int64(len(r.frame)),
			}
			p.verdict[i], p.state[i] = Applied, sp.after.Source
		case Extend:
			if m == nil || op.Offset != m.Size || op.HeadHash != m.HeadHash {
				p.verdict[i], p.state[i] = Conflict, stateOf(m)
				continue
			}
			sp := touch(r.key)
			sp.frames = append(sp.frames, r.frame)
			sp.after.Size += int64(len(op.Lines.b))
			sp.after.RawEnd = op.RawEnd
			sp.after.UpdatedMs = max(now(), sp.after.UpdatedMs+1)
			sp.after.ZstSize += int64(len(r.frame))
			p.verdict[i], p.state[i] = Applied, sp.after.Source
		case Backfill:
			if m == nil || op.Offset != m.Size || op.HeadHash != m.HeadHash {
				p.verdict[i], p.state[i] = Conflict, stateOf(m)
				continue
			}
			if op.RawEnd > 0 {
				sp := touch(r.key)
				sp.after.RawEnd = op.RawEnd
				m = &sp.after
			}
			p.verdict[i], p.state[i] = Applied, m.Source
		case serverAppend:
			prev := op.src
			prev.Day = op.src.Day.AddDate(0, 0, -1)
			if pm := cur(prev.key()); pm != nil && op.requestKey != "" && slices.Contains(pm.Applied, op.requestKey) {
				p.verdict[i], p.state[i] = Duplicate, pm.Source
				continue
			}
			if m != nil && op.requestKey != "" && slices.Contains(m.Applied, op.requestKey) {
				p.verdict[i], p.state[i] = Duplicate, m.Source
				continue
			}
			sp := touch(r.key)
			n := int64(len(op.lines.b))
			if m == nil {
				first, _, _ := bytes.Cut(op.lines.b, []byte{'\n'})
				sp.rewrite = true
				sp.frames = [][]byte{r.frame}
				sp.after = manifestBody{
					Source: wire.Source{
						SrcID:      r.key.SrcID,
						Harness:    r.key.Harness,
						SourceName: op.src.name(),
						Path:       op.src.name(),
						Size:       n,
						HeadHash:   wire.HeadHash(op.lines.b[:len(first)+1]),
						UpdatedMs:  now(),
					},
					ZstSize: int64(len(r.frame)),
				}
			} else {
				sp.frames = append(sp.frames, r.frame)
				sp.after.Size += n
				sp.after.UpdatedMs = max(now(), sp.after.UpdatedMs+1)
				sp.after.ZstSize += int64(len(r.frame))
			}
			if op.requestKey != "" {
				sp.after.Applied = append(sp.after.Applied, op.requestKey)
			}
			p.verdict[i], p.state[i] = Applied, sp.after.Source
		}
	}
	return p
}

func stateOf(m *manifestBody) wire.Source {
	if m == nil {
		return wire.Source{}
	}
	return m.Source
}

// written: W1 done, nothing synced.
type written struct {
	dataDir  string
	p        planned
	tmpRaw   []string
	tmpMan   []string
	touched  touched
	linkDirs []string
	unlinked []layout.Key
	tails    []*sourcePlan
}

func (p planned) write(dataDir string) (written, error) {
	w := written{dataDir: dataDir, p: p, tmpRaw: make([]string, len(p.srcs)), tmpMan: make([]string, len(p.srcs))}
	dirs := map[string]bool{}
	addDir := func(d string) {
		if !dirs[d] {
			dirs[d] = true
			w.linkDirs = append(w.linkDirs, d)
		}
	}
	for i, sp := range p.srcs {
		raw, man := sp.key.Raw(dataDir), sp.key.Manifest(dataDir)
		dir := filepath.Dir(man)
		created, err := mkdirs(dataDir, dir)
		if err != nil {
			return w, err
		}
		w.touched.dirs = append(w.touched.dirs, created...)
		addDir(dir)
		switch {
		case sp.rewrite:
			if err := os.Remove(man); err != nil && !errors.Is(err, os.ErrNotExist) {
				return w, err
			}
			w.unlinked = append(w.unlinked, sp.key)
			w.touched.dirs = append(w.touched.dirs, dir)
			tmp, err := writeTemp(raw, sp.frames)
			w.tmpRaw[i] = tmp
			if err != nil {
				return w, err
			}
			w.touched.files = append(w.touched.files, tmp)
		case len(sp.frames) > 0:
			w.tails = append(w.tails, sp)
			if err := appendFrames(raw, sp.before.ZstSize, sp.frames); err != nil {
				return w, err
			}
			w.touched.files = append(w.touched.files, raw)
		}
		b, err := json.Marshal(sp.after)
		if err != nil {
			return w, err
		}
		tmp, err := writeTemp(man, [][]byte{b, {'\n'}})
		w.tmpMan[i] = tmp
		if err != nil {
			return w, err
		}
		w.touched.files = append(w.touched.files, tmp)
	}
	return w, nil
}

// rollback undoes W1's raw tails and temps only; no link or barrier has run.
// Repeating it is safe, including after a partial cleanup. A crash partway
// through leaves only the tails, missing manifests and temps of rows 2-4.
// Empty directories are harmless and need not be removed.
func (w written) rollback() error {
	for _, sp := range w.tails {
		if err := os.Truncate(sp.key.Raw(w.dataDir), sp.before.ZstSize); err != nil {
			return err
		}
	}
	for _, temps := range [][]string{w.tmpRaw, w.tmpMan} {
		for _, path := range temps {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// Only allocation/write failures in W1 get this marker. Close, truncate,
// unlink, barrier, link and cleanup errors remain fatal, even for ENOSPC.
type writeSpaceError struct{ cause error }

func (e *writeSpaceError) Error() string { return "store: W1: " + e.cause.Error() }
func (e *writeSpaceError) Unwrap() error { return e.cause }

// Intake already maps ENOSPC to ResourceExhausted; preserve EDQUOT as well.
func (e *writeSpaceError) Is(target error) bool { return target == syscall.ENOSPC }

func writeError(err error) error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return &writeSpaceError{cause: err}
	}
	return err
}

// mkdirs makes dir and returns the dirs whose entries it changed, so the
// barrier can make the new entries durable.
func mkdirs(dataDir, dir string) ([]string, error) {
	if _, err := os.Stat(dir); err == nil {
		return nil, nil
	}
	var parents []string
	for d := dir; d != dataDir && d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			parents = append(parents, d)
			break
		}
		parents = append(parents, filepath.Dir(d))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, writeError(err)
	}
	return slices.Compact(parents), nil
}

// writeBatchFile is the W1 file-write seam; a negative offset means sequential IO.
var writeBatchFile = func(f *os.File, b []byte, off int64) (int, error) {
	if off < 0 {
		return f.Write(b)
	}
	return f.WriteAt(b, off)
}

func writeTemp(path string, parts [][]byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", writeError(err)
	}
	for _, b := range parts {
		if _, err = writeBatchFile(f, b, -1); err != nil {
			err = writeError(err)
			break
		}
	}
	if cerr := f.Close(); cerr != nil {
		return f.Name(), fmt.Errorf("store: close temp: %w", errors.Join(err, cerr))
	}
	// Return even a partial temp: rollback owns its removal, and a removal
	// error must reach stop rather than hiding behind the original ENOSPC.
	return f.Name(), err
}

func appendFrames(raw string, zst int64, frames [][]byte) error {
	f, err := os.OpenFile(raw, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err = f.Truncate(zst); err == nil {
		off := zst
		for _, b := range frames {
			if _, err = writeBatchFile(f, b, off); err != nil {
				err = writeError(err)
				break
			}
			off += int64(len(b))
		}
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("store: close raw: %w", errors.Join(err, cerr))
	}
	return err
}

// touched is what a barrier makes durable: written files, then dirs whose
// entries changed.
type touched struct {
	files []string
	dirs  []string
}

func (t touched) empty() bool { return len(t.files) == 0 && len(t.dirs) == 0 }

type barrier func(touched) error

func (b barrier) run(t touched) error {
	if t.empty() {
		return nil
	}
	slices.Sort(t.dirs)
	t.dirs = slices.Compact(t.dirs)
	if err := b(t); err != nil {
		return fmt.Errorf("store: barrier: %w", err)
	}
	return nil
}

// durableData: B1 passed; no rename yet.
type durableData struct{ w written }

func (w written) sync(b barrier) (durableData, error) {
	return durableData{w}, b.run(w.touched)
}

// rawsLinked: every rewrite's raw renamed into place and durable; no
// manifest renamed yet.
type rawsLinked struct{ d durableData }

func (d durableData) linkRaws(b barrier) (rawsLinked, error) {
	var t touched
	for i, sp := range d.w.p.srcs {
		if !sp.rewrite {
			continue
		}
		if err := os.Rename(d.w.tmpRaw[i], sp.key.Raw(d.w.dataDir)); err != nil {
			return rawsLinked{d}, err
		}
		t.dirs = append(t.dirs, filepath.Dir(d.w.tmpRaw[i]))
	}
	return rawsLinked{d}, b.run(t)
}

// committed: every manifest renamed and durable.
type committed struct {
	r       rawsLinked
	batch   uint64
	changed []layout.Key
}

func (r rawsLinked) linkManifests(b barrier, batch uint64) (committed, error) {
	w := r.d.w
	k := committed{r: r, batch: batch}
	for i, sp := range w.p.srcs {
		if err := os.Rename(w.tmpMan[i], sp.key.Manifest(w.dataDir)); err != nil {
			return k, err
		}
		k.changed = append(k.changed, sp.key)
	}
	return k, b.run(touched{dirs: w.linkDirs})
}

func (k committed) acks() []Ack {
	p := k.r.d.w.p
	out := make([]Ack, len(p.reqs))
	for i := range p.reqs {
		out[i] = Ack{verdict: p.verdict[i], state: p.state[i], batch: k.batch}
	}
	return out
}

func encodeFrame(l Lines) []byte {
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		panic(err)
	}
	if _, err := zw.Write(l.b); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// budget is a byte semaphore over queued frames.
type budget struct {
	mu    sync.Mutex
	avail int64
	freed chan struct{}
}

func newBudget(n int64) budget { return budget{avail: n, freed: make(chan struct{})} }

func (b *budget) acquire(ctx context.Context, n int64, stopped <-chan struct{}) error {
	for {
		b.mu.Lock()
		if b.avail >= n {
			b.avail -= n
			b.mu.Unlock()
			return nil
		}
		freed := b.freed
		b.mu.Unlock()
		select {
		case <-freed:
		case <-stopped:
			return errClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *budget) release(n int64) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	b.avail += n
	close(b.freed)
	b.freed = make(chan struct{})
	b.mu.Unlock()
}
