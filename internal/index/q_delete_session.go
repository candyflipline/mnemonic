package index

import (
	"context"
	"encoding/json"
)

type deleteSession struct{}

func init() { register(deleteSession{}) }

func (deleteSession) Name() string { return "delete_session" }

func (deleteSession) Call(ctx context.Context, db *DB, args json.RawMessage) (any, string, error) {
	panic("lane U5-controls")
}
