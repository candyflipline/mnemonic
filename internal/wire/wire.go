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
	RootsTo    int64  `json:"roots_to"` // redacted offset through which the client has reported roots; 0 = never
}

type RootKind string

const (
	RootGit RootKind = "git"
	RootDir RootKind = "dir"
)

// Root is one directory fact the pushing machine probed for a path its own
// redacted lines name. Path is the root as the transcript spells it; Key is
// the canonical git common dir (git) or the canonical Path (dir); Remote is
// the origin URL without userinfo (git only); From is the redacted offset of
// the first line of the source that named it.
type Root struct {
	Path   string   `json:"path"`
	Kind   RootKind `json:"kind"`
	Key    string   `json:"key"`
	Remote string   `json:"remote,omitempty"`
	From   int64    `json:"from"`
}

// POST /v1/roots/{machine}/{harness}/{src_id}?from=F&through=T
// Body: JSON []Root, at most MaxRootsPerPost, each From in [F, T). F == 0
// replaces the source's roots, F > 0 merges them. RootsTo advances to T when
// F <= RootsTo. Success is 200 with the updated Source.
const RootsPathPrefix = "/v1/roots/"

const MaxRootsPerPost = 256

const ManifestPath = "/v1/manifest"

// Manifest answers GET /v1/manifest?machine=M for the caller's author.
type Manifest struct {
	Author  string   `json:"author"`
	Machine string   `json:"machine"`
	Sources []Source `json:"sources"`
}

// PUT /v1/raw/{machine}/{harness}/{src_id}?offset=N&head_hash=H&source_name=S&path=P&raw_end=R
// Body: whole redacted lines, each ending in '\n'. An empty body only records
// raw_end: the server accepts it only at offset == Size > 0 with a matching
// head_hash (else 409, or 400 at offset 0). offset=0 replaces the raw file;
// offset>0 appends and must equal the server's Size, else 409 with a Conflict
// body. raw_end=R is the offset in the client's raw file the sent body ends
// at; R=0 (or absent) means unknown. Success is 200 with the updated Source.
const RawPathPrefix = "/v1/raw/"

// Conflict is the 409 body: the client re-reads Size and retries from there.
type Conflict struct {
	Size     int64  `json:"size"`
	HeadHash string `json:"head_hash"`
	RawEnd   int64  `json:"raw_end"`
}
