package store

// ARCHITECTURE SKETCH: no caller is migrated by this file.
import (
	"context"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/layout"
)

// SourceState is a committed position, independent of HTTP/protobuf encoding.
// ZstSize is private to the store's manifestBody, not part of this boundary.
type SourceState struct {
	Key                        layout.Key
	SourceName, Path, HeadHash string
	Size, RawEnd, UpdatedMS    int64
}

// UploadInput is untrusted until AcceptUpload checks identity, reserved names,
// offsets, whole lines and length. The body is one legacy PUT's body, possibly
// spooled; ownership transfers on success. It is never several merged frames.
type UploadInput struct {
	Machine, Harness, SrcID, HeadHash, SourceName, Path string
	Offset, RawEnd, BodyBytes                           int64
	Body                                                io.ReadCloser
}

type Mutation struct{ value *mutation }
type mutation struct {
	key  layout.Key
	body io.ReadCloser
	// TODO: closed union: replace | append-at-position | raw-end-backfill |
	// server-owned append (with existing current/previous-day requestKey rules).
}

func AcceptUpload(id auth.Identity, in UploadInput) (Mutation, error) { panic("not implemented") }
func AcceptServerAppend(src ServerSource, requestKey string, lines []byte) (Mutation, error) {
	panic("not implemented")
}

// Durable is sealed: external callers cannot construct a successful receipt.
// A nil interface is an error, never an ack; no exported receipt struct exists.
type Durable interface {
	State() SourceState
	Duplicate() bool
	durable()
}
type receipt struct {
	batch     *syncedBatch
	state     SourceState
	duplicate bool
}

func (*receipt) State() SourceState { panic("not implemented") }
func (*receipt) Duplicate() bool    { panic("not implemented") }
func (*receipt) durable()           { panic("not implemented") }

type Pending struct{ result <-chan commitResult }
type commitResult struct {
	receipt Durable
	err     error
}

func (*Pending) Wait(context.Context) (Durable, error) { panic("not implemented") }

// Committer is the only serve-mode writer. Its worker owns admission order,
// projected per-source states, machine leases and durable offset snapshots.
type Committer struct {
	root    string
	queue   chan *admission
	changes *Changes
	catalog map[layout.Key]manifestBody
}
type admission struct {
	mutation Mutation
	reply    chan commitResult
}

func OpenCommitter(root string) (*Committer, error) {
	// TODO: boot recovery/list once before accepting writes; reject nested mounts;
	// truncate raw tails to manifest ZstSize; discard dot temps and orphan raw;
	// sync recovered state, seed catalog and Changes, then start worker.
	panic("not implemented")
}
func (*Committer) Submit(context.Context, Mutation) (*Pending, error) {
	// TODO: reserve bounded queue bytes before reading; reject zero Mutation.
	// Cancellation before admission drops it; after admission outcome is unknown
	// until Wait succeeds or next RPC's durable snapshot resolves it.
	panic("not implemented")
}
func (*Committer) Close(context.Context) error { panic("not implemented") }
func (*Committer) Changes() *Changes           { panic("not implemented") }

// MachineLease serializes writers of one author's machine, not all clients.
// Open waits for admitted writes of a disconnected predecessor to settle.
type MachineLease struct {
	owner           *Committer
	author, machine string
}

func (*Committer) OpenMachine(context.Context, auth.Identity, string) (*MachineLease, error) {
	panic("not implemented")
}
func (*MachineLease) Snapshot() []SourceState                            { panic("not implemented") }
func (*MachineLease) Submit(context.Context, Mutation) (*Pending, error) { panic("not implemented") }
func (*MachineLease) Close() error                                       { panic("not implemented") }

// CommittedSource pins a raw inode and a compressed prefix under the source
// publication lock. Replacement renames cannot change this reader's inode.
type CommittedSource struct {
	file          *os.File
	state         SourceState
	compressedEnd int64
}

func (*Committer) OpenCommitted(layout.Key) (*CommittedSource, error) { panic("not implemented") }
func (*CommittedSource) State() SourceState                           { panic("not implemented") }
func (*CommittedSource) CompressedEnd() int64                         { panic("not implemented") }
func (*CommittedSource) ReadAt([]byte, int64) (int, error)            { panic("not implemented") }
func (*CommittedSource) Close() error                                 { panic("not implemented") }

// Distinct private phase types expose only the next legal durability step.
// No phase before syncedBatch has a receipt-producing method.
type collectingBatch struct{ items []*admission }
type stagedBatch struct {
	items []*admission
	files []*os.File
}
type invalidatedBatch struct{ staged *stagedBatch }
type installedBatch struct{ invalidated *invalidatedBatch }
type syncedBatch struct{ installed *installedBatch }

func (*collectingBatch) stage(context.Context) (*stagedBatch, error) {
	// TODO: fixed 8 ms from first admitted complete append, 128 operations or
	// 64 MiB logical body, whichever first; oversized singleton goes immediately.
	// Build one identical zstd frame per operation, one final manifest per key.
	panic("not implemented")
}
func (*stagedBatch) invalidateAndSync(context.Context) (*invalidatedBatch, error) {
	// TODO: unlink old manifests ONLY for replacements; barrier A flushes
	// staged frames, staged manifest contents, directory creation and unlinks.
	panic("not implemented")
}
func (*invalidatedBatch) installAndSync(context.Context) (*installedBatch, error) {
	// TODO: rename replacement raw files; barrier B makes their entries durable.
	// Appends already target existing raw inodes. Skip B if no raw renames.
	panic("not implemented")
}
func (*installedBatch) publishAndSync(context.Context) (*syncedBatch, error) {
	// TODO: rename final manifests; barrier C flushes their directory entries.
	// A/B failure forbids publication; any IO failure poisons committer until boot.
	panic("not implemented")
}
func (*syncedBatch) complete(*Committer) []Durable {
	// TODO: update durable catalog, mark Changes, then mint per-operation receipts
	// and release Pending waiters. Intermediate receipts name their own prefix.
	panic("not implemented")
}

// Changes is store-owned, in-memory, generation-tagged work, not a journal.
// Wakeups are hints. Pending keys are never dropped because a channel is full.
type Changes struct {
	mu      sync.Mutex
	next    uint64
	pending map[layout.Key]uint64
	wake    chan struct{}
}
type ChangeBatch struct {
	owner *Changes
	keys  map[layout.Key]uint64
}

func (*Changes) Wake() <-chan struct{} { panic("not implemented") }
func (*Changes) Take() *ChangeBatch {
	// TODO: under mutex swap pending with an empty map. A later append enters
	// that new map even if it names a key in the detached batch.
	panic("not implemented")
}
func (*ChangeBatch) Keys() []layout.Key { panic("not implemented") }
func (*ChangeBatch) Retry(keys []layout.Key) {
	// TODO: max-merge failed/unpublished generations into pending; success only
	// drops this detached batch, never deletes from the live pending map.
	panic("not implemented")
}

// LegacyPUT adapts validation/status mapping to Submit and Durable only.
// EXPIRES 2026-11-25: remove after the known-machine/token census is migrated;
// an extension requires an explicit owner decision, never a silent fallback.
func (*Committer) LegacyPUT() http.Handler { panic("not implemented") }
