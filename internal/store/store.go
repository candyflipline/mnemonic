package store

import (
	"encoding/json"
	"os"

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
