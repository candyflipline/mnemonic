// Package reader answers route calls over parts: in process on the keeper, or
// over HTTP on a stateless reader.
package reader

import (
	"github.com/candyflipline/mnemonic/arena/route"
	"github.com/candyflipline/mnemonic/arena/trace"
)

// Local is the keeper's in-process reader, ID route.LocalID, over the parts
// parts returns at each call, keyed by part ID.
func Local(parts func() map[string]*trace.Part) route.Reader {
	panic("lane L4")
}
