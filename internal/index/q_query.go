package index

import (
	"context"
	"encoding/json"
)

type queryTool struct{}

func init() { register(queryTool{}) }

func (queryTool) Name() string { return "query" }

func (queryTool) Call(ctx context.Context, db *DB, args json.RawMessage) (any, string, error) {
	panic("lane U6-query")
}
