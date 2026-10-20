// Package cache builds the derived DuckDB file every query reads. Owned by
// lane lake.
package cache

import (
	"database/sql"
	_ "embed"
)

// Schema is the contract between cache and query.
//
//go:embed schema.sql
var Schema string

type Stats struct {
	LakeFiles              int
	Events, Docs, Postings int64
	Path                   string
}

// Build writes cache-<ms>.duckdb.tmp from the lake, renames it, and keeps the
// newest two. It returns the zero Stats and no error when nothing changed
// since the newest cache.
func Build(dataDir string) (Stats, error) { panic("lane lake") }

// OpenNewest opens the newest complete cache read-only.
func OpenNewest(dataDir string) (*sql.DB, string, error) { panic("lane lake") }
