package store

// SKETCH (docs/design/uploader.md, "Group-commit state machine"): types and
// signatures only, bodies panic. Lane S makes Committer the Store: Store's
// DataDir, Wake, mu and locks fields, lockKey, replace, append, replaceRaw
// and the HTTP handlers (moved to internal/intake) are deleted, and
// Committer is renamed Store.

import (
	"context"
	"sync"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/wire"
)

// Batch bounds. A batch is whatever is queued when the committer finishes the
// previous one, up to these caps; there is no timer, so a lone append commits
// at once and a burst shares its barriers.
const (
	maxBatchAppends = 512
	maxBatchBytes   = 128 << 20
	// queueBytes bounds compressed frames submitted and not yet committed,
	// across every stream and the HTTP adapter. Submit blocks while it is
	// spent, which stops the stream's Recv, which closes the HTTP/2 window.
	queueBytes = 256 << 20
)

// Lines is a non-empty run of whole lines, each ending in '\n'. ParseLines is
// its only constructor, so nothing past the edge re-checks it.
type Lines struct{ b []byte }

func ParseLines(b []byte) (Lines, error) { panic("not implemented") }

// ClientSource is the key of a client-owned source: the author from auth,
// machine, harness and src_id valid segments, and none of them a
// server-owned name (ServerMachine, ServerHarnesses).
type ClientSource struct{ key layout.Key }

func NewClientSource(author, machine, harness, srcID string) (ClientSource, error) {
	panic("not implemented")
}

// Op is one client change to a source: exactly one of Replace, Extend,
// Backfill. ParseOp maps the PUT and stream fields onto it with today's
// servePut rules (store.go:112-148).
type Op interface{ isOp() }

// Replace makes the source exactly Lines (offset 0).
type Replace struct {
	HeadHash, SourceName, Path string
	RawEnd                     int64
	Lines                      Lines
}

// Extend appends Lines when the source is Offset long with HeadHash.
type Extend struct {
	Offset   int64
	HeadHash string
	RawEnd   int64
	Lines    Lines
}

// Backfill records RawEnd when the source is Offset long with HeadHash; no
// raw byte is written and Size and UpdatedMs stay.
type Backfill struct {
	Offset   int64
	HeadHash string
	RawEnd   int64
}

func (Replace) isOp()  {}
func (Extend) isOp()   {}
func (Backfill) isOp() {}

// serverAppend is Append's request: create or extend a server-owned source,
// deduplicated by requestKey against the source and the day before it.
type serverAppend struct {
	src        ServerSource
	requestKey string
	lines      Lines
}

func (serverAppend) isOp() {}

func ParseOp(offset int64, headHash, sourceName, path string, rawEnd int64, body []byte) (Op, error) {
	panic("not implemented")
}

// Verdict is what the committer did with one request.
type Verdict uint8

const (
	// Applied: the change is on disk; State is the source after it.
	Applied Verdict = iota + 1
	// Conflict: Offset or HeadHash did not match; nothing was written and
	// State is the source as stored (zero when there is none).
	Conflict
	// Duplicate: a server append whose request key was already applied.
	Duplicate
)

// Ack is a request's answer. Its fields are unexported and the zero Ack is
// invalid (Valid false), so outside this package an Ack exists only as
// Ticket.Wait returns it, and Wait returns only after the request's batch
// passed its last barrier. Inside, committed.acks is the one constructor
// (scripts/check: one `Ack{` literal in internal/store).
type Ack struct {
	verdict Verdict
	state   wire.Source
	batch   uint64
}

func (a Ack) Valid() bool        { return a.batch != 0 }
func (a Ack) Verdict() Verdict   { return a.verdict }
func (a Ack) State() wire.Source { return a.state }

// Ticket is one submitted request. Wait returns its Ack once the batch that
// carries it is durable, or the error that stopped the committer.
type Ticket struct {
	done chan struct{}
	ack  Ack
	err  error
}

func (t *Ticket) Wait(ctx context.Context) (Ack, error) { panic("not implemented") }

// Committer is the one writer of raw/: every client PUT, stream Append and
// server Append is a request on its queue, and one goroutine commits them in
// batches. It holds every source's manifest in memory (the table), loaded at
// Open; disk manifests are the table's persistence, written through.
type Committer struct {
	dataDir string
	reqs    chan *request
	budget  chan struct{} // TODO: a byte semaphore of queueBytes

	mu    sync.RWMutex
	table map[layout.Key]*manifestBody // written only by run, after a batch's last barrier

	changes changes
	seq     uint64 // batch number, from 1
	stopped chan struct{}
	err     error // set once when a barrier fails; every later Submit returns it
}

// request is a parsed, compressed change waiting for its batch.
type request struct {
	key    layout.Key
	op     Op
	frame  []byte // zstd frame of the op's Lines, encoded by Submit's caller
	ticket *Ticket
}

// Open loads the table from every raw manifest under dataDir, removes temp
// files a crash left (".<name>.json.*", ".<name>.jsonl.zst.*"), and starts
// the committer.
func Open(dataDir string) (*Committer, error) { panic("not implemented") }

// Close stops intake, commits what is queued, and returns the error that
// stopped the committer, if any.
func (c *Committer) Close() error { panic("not implemented") }

// Submit queues op for src and returns its ticket. It encodes the frame on
// the caller's goroutine (encodeFrame, the same zstd defaults as writeFrame,
// so frames are byte-identical to today's), then blocks while queueBytes is
// spent.
func (c *Committer) Submit(ctx context.Context, src ClientSource, op Op) (*Ticket, error) {
	panic("not implemented")
}

// Append is the server-owned write (OTLP, annotations, controls): submit and
// wait, same contract as today's Store.Append (append.go:73-83).
func (c *Committer) Append(src ServerSource, requestKey string, lines []byte) (_ wire.Source, duplicate bool, _ error) {
	panic("not implemented")
}

// Sources is author's sources on machine as the table holds them, sorted by
// SrcID: the stream's Welcome and the adapter's GET manifest. No disk read.
func (c *Committer) Sources(author, machine string) []wire.Source { panic("not implemented") }

// Changed fires after a batch that changed any source commits; TakeChanged
// then returns every key changed since the last take and clears the set.
func (c *Committer) Changed() <-chan struct{} { return c.changes.ready }

func (c *Committer) TakeChanged() []layout.Key { panic("not implemented") }

// changes is the dirty set: run adds a batch's keys after its last barrier,
// the index cycle takes the whole set by swap. Two appends racing a take land
// either in the taken set or in the next one, never in neither.
type changes struct {
	mu    sync.Mutex
	keys  map[layout.Key]struct{}
	ready chan struct{} // cap 1, non-blocking send after each add
}

// run is the committer loop.
func (c *Committer) run() {
	// TODO:
	// for {
	//   first := <-c.reqs (or stop)
	//   reqs := [first] + every request already queued, up to maxBatchAppends
	//           and maxBatchBytes (no wait)
	//   p := plan(c.table, reqs, nowMs)                  // pure
	//   w, err := p.write(c.dataDir)                     // W1
	//   d, err := w.sync(c.barrier)                      // B1
	//   r, err := d.linkRaws(c.barrier)                  // W2a (+B2 if any rewrite)
	//   k, err := r.linkManifests(c.barrier, c.seq+1)    // W2b + B3
	//   on any err: c.err = err; fail every ticket of this batch and every
	//     later Submit; never touch the table again (the process exits; boot
	//     reloads the truth from disk)
	//   c.mu.Lock(); apply k.table into c.table; c.mu.Unlock()
	//   c.changes.add(k.changed)
	//   for i, a := range k.acks() { reqs[i].ticket resolve a }
	//   release reqs' bytes from the budget
	// }
	panic("not implemented")
}

// The batch states. Each transition takes the previous state and returns the
// next; only the last returns committed, and only committed makes Acks.

// planned is the batch decided in memory: per request a verdict, per touched
// source its before and after manifest and the frames to write.
type planned struct {
	reqs    []*request
	verdict []Verdict
	srcs    map[layout.Key]*sourcePlan
}

// sourcePlan is one source's change in the batch. rewrite is a Replace or a
// server source's create: a new raw file from frames, the old manifest
// unlinked first. Otherwise the raw is truncated to before.ZstSize and the
// frames appended. Several requests on one source fold into one plan, in
// queue order: the frames concatenate, the manifest is written once.
type sourcePlan struct {
	key     layout.Key
	before  *manifestBody // nil when the source has no manifest
	after   manifestBody
	rewrite bool
	frames  [][]byte
}

// plan decides every request against the table as the batch's earlier
// requests left it. Pure: no IO, now is the clock.
func plan(table map[layout.Key]*manifestBody, reqs []*request, now func() int64) planned {
	// TODO per request, in order, against cur := table overlaid with this
	// batch's plans so far:
	//   Replace: verdict Applied; plan.rewrite, frames = [frame];
	//            after = Source{Size: len, HeadHash, SourceName, Path, RawEnd,
	//            UpdatedMs: max(now, prev+1)}, ZstSize derived from frames.
	//   Extend:  cur nil -> Conflict(zero); Offset/HeadHash mismatch ->
	//            Conflict(cur.Source); else Applied, frames += frame,
	//            Size += len, RawEnd, UpdatedMs = max(now, prev+1), ZstSize +=.
	//   Backfill: same checks; Applied; RawEnd recorded only when > 0.
	//   serverAppend: Duplicate when the key is in this or the day before's
	//            Applied; else create (rewrite) or extend, Applied += key.
	panic("not implemented")
}

// written: W1 done, nothing synced. Extends truncated to before.ZstSize and
// appended; rewrites' old manifests unlinked and new raws in temp files; every
// after-manifest in a temp file.
type written struct {
	p       planned
	touched touched
}

func (p planned) write(dataDir string) (written, error) { panic("not implemented") }

// touched is what a barrier must make durable: files written and dirs whose
// entries changed. Linux ignores it (syncfs); other systems fsync each.
type touched struct {
	files []string
	dirs  []string
}

// barrier makes touched durable. barrier_linux.go: one syncfs(2) on the data
// dir. barrier_other.go: fsync each file then each dir (darwin: F_FULLFSYNC
// through os.File.Sync).
type barrier func(touched) error

// durableData: B1 passed. Frames, temp raws, temp manifests and unlinks are
// on disk; no rename has happened.
type durableData struct{ w written }

func (w written) sync(b barrier) (durableData, error) { panic("not implemented") }

// rawsLinked: every rewrite's temp raw renamed over its raw, and B2 passed
// when there was any; no manifest renamed yet.
type rawsLinked struct{ d durableData }

func (d durableData) linkRaws(b barrier) (rawsLinked, error) { panic("not implemented") }

// committed: every temp manifest renamed and B3 passed. The batch is durable.
type committed struct {
	r       rawsLinked
	batch   uint64
	changed []layout.Key
}

func (r rawsLinked) linkManifests(b barrier, batch uint64) (committed, error) {
	panic("not implemented")
}

// acks is the only place an Ack is made.
func (k committed) acks() []Ack { panic("not implemented") }

// encodeFrame is writeFrame's zstd encoding into memory: the same
// zstd.NewWriter defaults, one Write, Close.
func encodeFrame(l Lines) []byte { panic("not implemented") }
