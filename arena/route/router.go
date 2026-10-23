package route

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/candyflipline/mnemonic/arena/keeper"
	"github.com/candyflipline/mnemonic/arena/obj"
)

// Coverage says what an Answer covers.
type Coverage string

const (
	// CoverAll is the committed parts plus the keeper's hot parts for that
	// manifest.
	CoverAll Coverage = "all"
	// CoverCommitted is the committed manifest only: while a source is hot its
	// whole component is absent from the manifest, so sessions active within
	// the keeper's -quiet window can be missing entirely, not just their newest
	// events.
	CoverCommitted Coverage = "committed"
)

var (
	ErrStale    = errors.New("route: no manifest confirmed latest within DeleteSlack/2")
	ErrNoKeeper = errors.New("route: no keeper for the whole rung")
)

// KeeperHead is the keeper's GET /v1/head: the manifest it committed at its own
// gen, and the hot parts that complement exactly that manifest. The keeper
// answers 503 until its first commit at its gen.
type KeeperHead struct {
	Gen keeper.Gen
	Seq keeper.Seq
	Hot []keeper.PartRef
}

// Keeper is the router's link to the writer: a Reader with ID LocalID that
// answers the keeper's hot parts and the Whole rung, plus its head.
type Keeper interface {
	Reader
	Head(ctx context.Context) (KeeperHead, error)
}

// HTTPKeeper is the Keeper at baseURL; Head fails within headTimeout, Call
// within callTimeout.
func HTTPKeeper(baseURL string, headTimeout, callTimeout time.Duration) Keeper {
	panic("lane L3")
}

type Answer struct {
	Data     json.RawMessage
	Text     string
	Gen      keeper.Gen
	Seq      keeper.Seq
	Coverage Coverage
}

// Cursor is a reader's place in the view stream: the manifest it holds and
// when its grants expire. Its wire form is %016x-%016x-<unix ms>.
type Cursor struct {
	Gen     keeper.Gen
	Seq     keeper.Seq
	Expires time.Time
}

func (c Cursor) String() string { panic("lane L3") }

func ParseCursor(s string) (Cursor, error) { panic("lane L3") }

type Router struct {
	tenant  string
	s       obj.ReadStore
	k       Keeper
	readers []Reader
	listed  atomic.Pointer[listed]
	changed atomic.Pointer[chan struct{}]
}

// listed is the highest (gen, seq) a List showed, and when that List started.
type listed struct {
	m  keeper.Manifest
	at time.Time
}

// NewRouter routes tenant's calls over s's manifests; k may be nil.
func NewRouter(tenant string, s obj.ReadStore, k Keeper, readers []Reader) *Router {
	panic("lane L3")
}

// Run lists m/ every min(2s, DeleteSlack/6) until ctx ends; a List that fails
// leaves listed alone, so its age decides staleness.
func (r *Router) Run(ctx context.Context) error { panic("lane L3") }

// Call answers one tool call over the latest listed manifest, paired with the
// keeper's hot parts when the keeper's head matches it.
func (r *Router) Call(ctx context.Context, rung Rung, tool string, args json.RawMessage) (Answer, error) {
	panic("lane L3")
}

// View is GET /v1/view?after=: the served manifest's cold parts freshly
// granted, once the manifest is above after's or after's grants are within a
// third of GrantTTL of expiry, waiting up to 30 s; ok is false when neither
// happened.
func (r *Router) View(ctx context.Context, after Cursor) (v View, ok bool, err error) {
	panic("lane L3")
}
