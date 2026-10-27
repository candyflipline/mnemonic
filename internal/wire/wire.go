// Package wire is the push protocol between `mnemonic push` and `mnemonic
// serve`. The server's manifest is the only sync state: a client appends from
// the server's size when the source's first line is unchanged and the file
// grew, and re-sends the whole file otherwise.
package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

const HeadCap = 64 << 10

var segmentRe = regexp.MustCompile(`^[a-z0-9._-]+$`)

func ValidSegment(s string) bool {
	return s != "." && s != ".." && segmentRe.MatchString(s)
}

// SrcID names a source file on the server: the first 16 hex chars of
// sha256(absolute path on the client).
func SrcID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:8])
}

// HeadHash identifies a source's content: sha256 of its first line, newline
// included, capped at HeadCap bytes. A rewritten file changes it.
func HeadHash(firstLine []byte) string {
	if len(firstLine) > HeadCap {
		firstLine = firstLine[:HeadCap]
	}
	sum := sha256.Sum256(firstLine)
	return hex.EncodeToString(sum[:])
}

// Source is one raw file as the server holds it. Size counts redacted bytes
// received, which is the offset the next append must carry.
type Source struct {
	SrcID      string `json:"src_id"`
	Harness    string `json:"harness"`
	SourceName string `json:"source_name"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	RawEnd     int64  `json:"raw_end"` // raw bytes of the client's file consumed into Size; 0 = unknown
	HeadHash   string `json:"head_hash"`
	UpdatedMs  int64  `json:"updated_ms"`
}
