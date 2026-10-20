// Package push is the client side of the wire protocol: scan the configured
// globs, diff against the server's manifest, send only whole redacted lines.
// Owned by lane push.
package push

import (
	"context"
	"net/http"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/redact"
)

type Stats struct {
	Scanned, Appended, Replaced, Unchanged, Failed int
	Bytes                                          int64
}

// Run does one pass. Per source: head hash equal and the redacted size grew
// means append from the server's Size; otherwise send the whole file with
// offset 0. A trailing line without '\n' waits for the next pass. A 409
// re-reads the manifest once for that source. Rerunning with no new bytes
// sends nothing.
func Run(ctx context.Context, cfg config.Client, red *redact.Redactor, hc *http.Client) (Stats, error) {
	panic("lane push")
}
