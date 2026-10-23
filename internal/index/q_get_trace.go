package index

import (
	"context"
	"encoding/json"
)

type getTrace struct{}

func init() { register(getTrace{}) }

func (getTrace) Name() string { return "get_trace" }

func (getTrace) Call(ctx context.Context, db *DB, args json.RawMessage) (any, string, error) {
	panic("lane U5-spans")
}
