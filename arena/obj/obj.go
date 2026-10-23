// Package obj is the object store parts, manifests and generation claims live
// in: a local dir or an S3-compatible bucket.
package obj

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
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

// ReadStore is the part of a Store a router needs: it lists and presigns,
// never writes.
type ReadStore interface {
	// List returns every key under prefix, sorted, strongly consistent with
	// every Put and PutExclusive that returned before it.
	List(ctx context.Context, prefix Key) ([]Entry, error)
	// PresignGetUntil returns a URL that GETs k, Range allowed, until notAfter,
	// with no credentials on the reader's side. The expiry is absolute so a
	// late caller cannot mint a later window than the one it was given.
	PresignGetUntil(k Key, notAfter time.Time) (string, error)
}

type Store interface {
	ReadStore
	// Put writes size bytes from r under k.
	Put(ctx context.Context, k Key, r io.Reader, size int64) error
	// PutExclusive writes k only if no object has it, and returns ErrExists
	// otherwise.
	PutExclusive(ctx context.Context, k Key, r io.Reader, size int64) error
	// Delete removes keys; a missing key is not an error.
	Delete(ctx context.Context, keys []Key) error
}

// Open opens file:///abs/dir?serve=http://127.0.0.1:PORT or
// s3://bucket/prefix?endpoint=HOST&region=R, with S3 credentials from the env.
func Open(uri string) (Store, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("obj: %w", err)
	}
	switch u.Scheme {
	case "file":
		return openFile(u)
	case "s3":
		return openS3(u)
	}
	return nil, fmt.Errorf("obj: scheme %q is not file or s3", u.Scheme)
}

// OpenRead opens uri as Open does, for a process that only lists and presigns.
func OpenRead(uri string) (ReadStore, error) {
	return Open(uri)
}

// Serve serves a file store's presigned GETs at its serve= address until ctx
// ends; for an s3 store it returns nil at once.
func Serve(ctx context.Context, s Store) error {
	f, ok := s.(*fileStore)
	if !ok {
		return nil
	}
	return f.serveHTTP(ctx)
}

const tmpPrefix = ".tmp-"

func checkKey(k Key, prefix bool) error {
	s := string(k)
	switch {
	case s == "" && prefix:
		return nil
	case s == "", strings.Contains(s, ".."), strings.HasPrefix(s, "/"), strings.Contains(s, "\x00"),
		strings.HasPrefix(s, tmpPrefix), strings.Contains(s, "/"+tmpPrefix),
		!prefix && strings.HasSuffix(s, "/"):
		return fmt.Errorf("obj: bad key %q", s)
	}
	return nil
}
