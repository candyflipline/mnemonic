package trace

import (
	"errors"
	"io"
)

// ErrNoParseRecord: the part predates partFormat 4, so its members parse from
// raw.
var ErrNoParseRecord = errors.New("trace: part has no parse records")

// RestorePart makes store cover part id as the keeper that built it did:
// parts/<id>/ unpacked from the blob fetch streams (checked against
// sha256hex, skipped when the dir exists), and for every member with no
// checkpoint, logs re-framed from the part's own frames plus a checkpoint
// marked restored. It reports how many members it restored. Safe to rerun
// after a crash at any point.
func RestorePart(store, id, sha256hex string, fetch func(io.Writer) error) (restored int, err error) {
	panic("lane L1")
}
