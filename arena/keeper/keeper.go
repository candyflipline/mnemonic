// Package keeper is the one writer per tenant: it claims a generation and
// commits manifests to the object store.
//
// Keys:
//
//	t/<tenant>/g/<%016x gen>
//	t/<tenant>/m/<%016x gen>-<%016x seq>.json
//	t/<tenant>/p/<id>.g<gen>
package keeper

import (
	"context"
	"encoding/json"

	"github.com/candyflipline/mnemonic/arena/obj"
)

type Gen uint64

type Seq uint64

type PartRef struct {
	ID         string
	Gen        Gen
	Key        obj.Key
	Size       int64
	SHA256     string
	Sessions   []string
	ScoredDocs int64
	TotalLen   int64
}

type Manifest struct {
	Tenant  string
	Gen     Gen
	Seq     Seq
	Parts   []PartRef
	Sources json.RawMessage
}

// Latest reads the manifest with the highest (gen, seq); ok is false when the
// tenant has none.
func Latest(ctx context.Context, s obj.Store, tenant string) (m Manifest, ok bool, err error) {
	panic("lane L3")
}

// Claim creates g/<gen> exclusively for the next gen above every claimed one,
// re-listing and trying the next gen on ErrExists.
func Claim(ctx context.Context, s obj.Store, tenant string) (Gen, error) {
	panic("lane L3")
}

// Current reports whether gen is still the highest claimed gen.
func Current(ctx context.Context, s obj.Store, tenant string, gen Gen) (bool, error) {
	panic("lane L3")
}
