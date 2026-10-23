package index

import (
	"context"
	"encoding/json"
)

type setVisibility struct{}

func init() { register(setVisibility{}) }

func (setVisibility) Name() string { return "set_visibility" }

func (setVisibility) Call(ctx context.Context, db *DB, args json.RawMessage) (any, string, error) {
	panic("lane U5-controls")
}
