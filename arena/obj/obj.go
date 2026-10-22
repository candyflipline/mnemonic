// Package obj is the object store parts, manifests and generation claims live
// in: a local dir or an S3-compatible bucket.
package obj

import (
	"context"
	"errors"
	"io"
	"time"
)

// Key names an object. Objects are immutable: a key is written once and never
// rewritten with other bytes.
type Key string

var (
	ErrExists   = errors.New("obj: key exists")
	ErrNotFound = errors.New("obj: key not found")
)

type Entry struct {
	Key  Key
	Size int64
}

type Store interface {
	// Put writes size bytes from r under k.
	Put(ctx context.Context, k Key, r io.Reader, size int64) error
	// PutExclusive writes k only if no object has it, and returns ErrExists
	// otherwise.
	PutExclusive(ctx context.Context, k Key, r io.Reader, size int64) error
	// List returns every key under prefix, sorted, strongly consistent with
	// every Put and PutExclusive that returned before it.
	List(ctx context.Context, prefix Key) ([]Entry, error)
	// Delete removes keys; a missing key is not an error.
	Delete(ctx context.Context, keys []Key) error
	// PresignGet returns a URL that GETs k, Range allowed, for ttl, with no
	// credentials on the reader's side.
	PresignGet(k Key, ttl time.Duration) (string, error)
}

// Open opens file:///abs/dir?serve=http://127.0.0.1:PORT or
// s3://bucket/prefix?endpoint=HOST&region=R, with S3 credentials from the env.
func Open(uri string) (Store, error) {
	panic("lane L1")
}

// Serve serves a file store's presigned GETs at its serve= address until ctx
// ends; for an s3 store it returns nil at once.
func Serve(ctx context.Context, s Store) error {
	panic("lane L1")
}
