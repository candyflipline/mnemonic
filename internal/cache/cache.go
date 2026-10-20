// Package cache builds the derived DuckDB file every query reads. Owned by
// lane cache.
package cache

import (
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
