package store

import (
	"encoding/json"
	"os"

	"github.com/candyflipline/mnemonic/internal/layout"
	"github.com/candyflipline/mnemonic/internal/wire"
)

// manifestBody is what a Key.Manifest file holds: the wire.Source the protocol
// answers with, plus zst_size, the raw-file length Source.Size is known to
// cover, and the request keys a server source applied.
type manifestBody struct {
	wire.Source
	ZstSize int64    `json:"zst_size"`
	Applied []string `json:"applied,omitempty"`
}

func readManifest(path string) (*manifestBody, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m manifestBody
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Manifest is one committed source as the table holds it: what the index
// needs from the manifest file, so boot does not read the file again.
type Manifest struct {
	Key     layout.Key
	Source  wire.Source
	ZstSize int64
}

// Manifests is every source in the table, in no order. After Open it is what
// loadTable read; the committer keeps it current as batches commit.
func (c *Committer) Manifests() []Manifest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Manifest, 0, len(c.table))
	for k, m := range c.table {
		out = append(out, Manifest{Key: k, Source: m.Source, ZstSize: m.ZstSize})
	}
	return out
}
