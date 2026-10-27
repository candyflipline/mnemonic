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

func SrcID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:8])
}

func HeadHash(firstLine []byte) string {
	if len(firstLine) > HeadCap {
		firstLine = firstLine[:HeadCap]
	}
	sum := sha256.Sum256(firstLine)
	return hex.EncodeToString(sum[:])
}

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
