// Package route fans a tool call out to the readers that own its parts and
// merges their answers.
package route

import (
	"context"
	"encoding/json"

	"github.com/candyflipline/mnemonic/arena/keeper"
)

type Rung int

const (
	Whole Rung = iota
	Partial
)

// LocalID is the ID of the keeper's in-process reader; Local parts go only to
// it.
const LocalID = "local"

type Reader interface {
	ID() string
	Call(ctx context.Context, req CallRequest) (CallResponse, error)
}

type GrantedPart struct {
	keeper.PartRef
	URL   string
	Local bool
}

type View struct {
	Tenant string
	Gen    keeper.Gen
	Seq    keeper.Seq
	Parts  []GrantedPart
}

type CallRequest struct {
	Tenant string
	Parts  []GrantedPart
	Tool   string
	Args   json.RawMessage
	Phase  string
	Stats  json.RawMessage
}

type CallResponse struct {
	Data  json.RawMessage
	Text  string
	Phase json.RawMessage
}

// Owner ranks readerIDs for partID by rendezvous hash, owner first.
func Owner(partID string, readerIDs []string) []string {
	panic("lane L4")
}

// Do answers one tool call over v at rung.
func Do(ctx context.Context, v View, readers []Reader, rung Rung, tool string, args json.RawMessage) (json.RawMessage, string, error) {
	panic("lane L4")
}

// scatter sends every Local part to the reader whose ID is LocalID and every
// other part to its rendezvous owner (Owner over the reader IDs), one
// CallRequest per reader carrying that reader's parts. On a reader error it
// reassigns that reader's parts to their next rank and retries once.
func scatter(ctx context.Context, v View, readers []Reader, tool, phase string, args, stats json.RawMessage) ([]CallResponse, error) {
	panic("lane L4")
}

// HTTPReader is a Reader that calls the reader server at baseURL.
func HTTPReader(id, baseURL string) Reader {
	panic("lane L4")
}
