package index

import (
	"context"
	"encoding/json"
)

type annotate struct{}

func init() { register(annotate{}) }

func (annotate) Name() string { return "annotate" }

func (annotate) Call(ctx context.Context, db *DB, args json.RawMessage) (any, string, error) {
	panic("lane U5-annotations")
}
