// Package store is the server side of the wire protocol. Owned by lane store.
package store

import "net/http"

// Store holds raw sources under DataDir per internal/layout.
type Store struct {
	DataDir string
}

// Handler serves GET /v1/manifest and PUT /v1/raw/... for the author that
// auth.Middleware put on the context. Writes go temp then rename; an append
// whose offset differs from Size is a 409 wire.Conflict.
func (s *Store) Handler() http.Handler { panic("lane store") }
