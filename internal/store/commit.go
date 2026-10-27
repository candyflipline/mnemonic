package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type Lines struct{ b []byte }

var errLines = errors.New("body must be non-empty whole lines")

func ParseLines(b []byte) (Lines, error) {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return Lines{}, errLines
	}
	return Lines{b}, nil
}

var ErrReserved = errors.New("machine or harness is reserved for server-owned sources")

var errSegment = errors.New("bad path segment")

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

type Backfill struct {
	Offset   int64
	HeadHash string
	RawEnd   int64
}

func (Replace) isOp()  {}
func (Extend) isOp()   {}
func (Backfill) isOp() {}

type queuedOp interface{ queued() }

type replaceMeta struct {
	headHash, sourceName, path string
	rawEnd, linesLen           int64
}

type extendMeta struct {
	offset, rawEnd, linesLen int64
	headHash                 string
}

type serverAppend struct {
	src        ServerSource
	requestKey string
	linesLen   int64
	firstHash  string
}

type flush struct{}

func (replaceMeta) queued()  {}
func (extendMeta) queued()   {}
func (Backfill) queued()     {}
func (serverAppend) queued() {}
func (flush) queued()        {}

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
	Applied Verdict = iota + 1
	Conflict
	Duplicate
)

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

func (t *Ticket) Wait(ctx context.Context) (a Ack, err error) {
	select {
	case <-t.done:
		return t.ack, t.err
	case <-ctx.Done():
		return a, ctx.Err()
	}
}

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
	op     queuedOp
	frame  []byte
	ticket *Ticket
}

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
	table, err := loadDurableTable(dataDir, b)
	if err != nil {
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

func loadDurableTable(dataDir string, b barrier) (map[layout.Key]*manifestBody, error) {
	table := map[layout.Key]*manifestBody{}
	var loaded touched
	root := filepath.Join(dataDir, "raw")
	authors, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return table, nil
	}
	if err != nil {
		return nil, err
	}
	for _, a := range authors {
		author, ok := strings.CutPrefix(a.Name(), "author=")
		if !ok || !a.IsDir() {
			continue
		}
		machines, err := os.ReadDir(filepath.Join(root, a.Name()))
		if err != nil {
			return nil, err
		}
		for _, m := range machines {
			machine, ok := strings.CutPrefix(m.Name(), "machine=")
			if !ok || !m.IsDir() {
				continue
			}
			harnesses, err := os.ReadDir(filepath.Join(root, a.Name(), m.Name()))
			if err != nil {
				return nil, err
			}
			for _, h := range harnesses {
				harness, ok := strings.CutPrefix(h.Name(), "harness=")
				if !ok || !h.IsDir() {
					continue
				}
				dir := filepath.Join(root, a.Name(), m.Name(), h.Name())
				if err := loadDir(table, &loaded, dir, layout.Key{Author: author, Machine: machine, Harness: harness}); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := b.run(loaded); err != nil {
		return nil, err
	}
	return table, nil
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

func (c *Committer) Submit(ctx context.Context, src ClientSource, op Op) (*Ticket, error) {
	switch o := op.(type) {
	case Replace:
		m := replaceMeta{headHash: o.HeadHash, sourceName: o.SourceName, path: o.Path, rawEnd: o.RawEnd, linesLen: int64(len(o.Lines.b))}
		return c.submit(ctx, src.key, m, encodeFrame(o.Lines))
	case Extend:
		m := extendMeta{offset: o.Offset, headHash: o.HeadHash, rawEnd: o.RawEnd, linesLen: int64(len(o.Lines.b))}
		return c.submit(ctx, src.key, m, encodeFrame(o.Lines))
	case Backfill:
		return c.submit(ctx, src.key, o, nil)
	}
	return nil, fmt.Errorf("store: op %T is not a client op", op)
}

func (c *Committer) Flush(ctx context.Context) {
	if t, err := c.submit(ctx, layout.Key{}, flush{}, nil); err == nil {
		t.Wait(ctx)
	}
}

func (c *Committer) submit(ctx context.Context, k layout.Key, op queuedOp, frame []byte) (*Ticket, error) {
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

func (c *Committer) Append(src ServerSource, requestKey string, lines []byte) (_ wire.Source, duplicate bool, _ error) {
	if err := src.validate(); err != nil {
		return wire.Source{}, false, err
	}
	l, err := ParseLines(lines)
	if err != nil {
		return wire.Source{}, false, fmt.Errorf("store: %w", err)
	}
	first, _, _ := bytes.Cut(l.b, []byte{'\n'})
	op := serverAppend{
		src:        src,
		requestKey: requestKey,
		linesLen:   int64(len(l.b)),
		firstHash:  wire.HeadHash(l.b[:len(first)+1]),
	}
	ctx := context.Background()
	t, err := c.submit(ctx, src.key(), op, encodeFrame(l))
	if err != nil {
		return wire.Source{}, false, err
	}
	a, err := t.Wait(ctx)
	if err != nil {
		return wire.Source{}, false, err
	}
	return a.State(), a.Verdict() == Duplicate, nil
}

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

func (c *Committer) Changed() <-chan struct{} { return c.changes.ready }

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
		// The budget no longer counts this prefix. Drop its references before
		// re-slicing: the backing array remains live even while we are idle.
		clear(held[:n])
		held = held[n:]
	}
}

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

func (c *Committer) commit(reqs []*request) (int, error) {
	p := plan(c.table, reqs, func() int64 { return time.Now().UnixMilli() })
	s, err := p.write(c.dataDir)
	if err != nil {
		if _, ok := err.(*w1NoSpace); !ok {
			return 0, err
		}
		if cleanupErr := s.rollback(); cleanupErr != nil {
			return 0, fmt.Errorf("store: W1 rollback: %w", errors.Join(err, cleanupErr))
		}
		for _, req := range p.reqs {
			req.ticket.fail(err)
		}
		c.budget.release(int64(size(p.reqs)))
		return len(p.reqs), nil
	}
	w, err := s.unlinkReplaced()
	if err != nil {
		return 0, err
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

type planned struct {
	reqs    []*request
	verdict []Verdict
	state   []wire.Source
	srcs    []*sourcePlan
}

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
		if overwritesPlanned(bySrc, r) {
			p.reqs, p.verdict, p.state = reqs[:i], p.verdict[:i], p.state[:i]
			return p
		}
		m := cur(r.key)
		switch op := r.op.(type) {
		case replaceMeta:
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
					SourceName: op.sourceName,
					Path:       op.path,
					Size:       op.linesLen,
					RawEnd:     op.rawEnd,
					HeadHash:   op.headHash,
					UpdatedMs:  max(now(), prevMs+1),
				},
				ZstSize: int64(len(r.frame)),
			}
			p.verdict[i], p.state[i] = Applied, sp.after.Source
		case extendMeta:
			if m == nil || op.offset != m.Size || op.headHash != m.HeadHash {
				p.verdict[i], p.state[i] = Conflict, stateOf(m)
				continue
			}
			sp := touch(r.key)
			sp.frames = append(sp.frames, r.frame)
			sp.after.Size += op.linesLen
			sp.after.RawEnd = op.rawEnd
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
			n := op.linesLen
			if m == nil {
				sp.rewrite = true
				sp.frames = [][]byte{r.frame}
				sp.after = manifestBody{
					Source: wire.Source{
						SrcID:      r.key.SrcID,
						Harness:    r.key.Harness,
						SourceName: op.src.name(),
						Path:       op.src.name(),
						Size:       n,
						HeadHash:   op.firstHash,
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

func overwritesPlanned(bySrc map[layout.Key]*sourcePlan, r *request) bool {
	_, replace := r.op.(replaceMeta)
	return replace && bySrc[r.key] != nil
}

func stateOf(m *manifestBody) wire.Source {
	if m == nil {
		return wire.Source{}
	}
	return m.Source
}

type written struct {
	dataDir  string
	p        planned
	tmpRaw   []string
	tmpMan   []string
	touched  touched
	linkDirs []string
	tails    []*sourcePlan
}

func (p planned) write(dataDir string) (staged, error) {
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
			return staged{w}, err
		}
		w.touched.dirs = append(w.touched.dirs, created...)
		addDir(dir)
		switch {
		case sp.rewrite:
			if err := writeTemp(raw, sp.frames, &w.tmpRaw[i]); err != nil {
				return staged{w}, err
			}
			w.touched.files = append(w.touched.files, w.tmpRaw[i])
		case len(sp.frames) > 0:
			w.tails = append(w.tails, sp)
			if err := appendFrames(raw, sp.before.ZstSize, sp.frames); err != nil {
				return staged{w}, err
			}
			w.touched.files = append(w.touched.files, raw)
		}
		b, err := json.Marshal(sp.after)
		if err != nil {
			return staged{w}, err
		}
		if err := writeTemp(man, [][]byte{b, {'\n'}}, &w.tmpMan[i]); err != nil {
			return staged{w}, err
		}
		w.touched.files = append(w.touched.files, w.tmpMan[i])
	}
	return staged{w}, nil
}

type staged struct{ w written }

func (s staged) unlinkReplaced() (written, error) {
	w := s.w
	for _, sp := range w.p.srcs {
		if !sp.rewrite {
			continue
		}
		man := sp.key.Manifest(w.dataDir)
		if err := os.Remove(man); err != nil && !errors.Is(err, os.ErrNotExist) {
			return w, err
		}
		w.touched.dirs = append(w.touched.dirs, filepath.Dir(man))
	}
	return w, nil
}

func (s staged) rollback() error {
	for _, sp := range s.w.tails {
		if err := os.Truncate(sp.key.Raw(s.w.dataDir), sp.before.ZstSize); err != nil {
			return err
		}
	}
	for _, temps := range [][]string{s.w.tmpRaw, s.w.tmpMan} {
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

var ErrNoSpace = errors.New("store: no space")

type w1NoSpace struct{ cause error }

func (e *w1NoSpace) Error() string        { return "store: W1: " + e.cause.Error() }
func (e *w1NoSpace) Unwrap() error        { return e.cause }
func (e *w1NoSpace) Is(target error) bool { return target == ErrNoSpace }

func writeError(err error) error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return &w1NoSpace{cause: err}
	}
	return err
}

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

var writeBatchFile = (*os.File).Write

func writeTemp(path string, parts [][]byte, name *string) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return writeError(err)
	}
	*name = f.Name()
	err = writeParts(f, parts)
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("store: close temp: %w", errors.Join(err, cerr))
	}
	return err
}

func appendFrames(raw string, zst int64, frames [][]byte) error {
	f, err := os.OpenFile(raw, os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err = f.Truncate(zst); err == nil {
		if _, err = f.Seek(zst, io.SeekStart); err == nil {
			err = writeParts(f, frames)
		}
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("store: close raw: %w", errors.Join(err, cerr))
	}
	return err
}

func writeParts(f *os.File, parts [][]byte) error {
	for _, b := range parts {
		if _, err := writeBatchFile(f, b); err != nil {
			return writeError(err)
		}
	}
	return nil
}

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

type durableData struct{ w written }

func (w written) sync(b barrier) (durableData, error) {
	return durableData{w}, b.run(w.touched)
}

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
