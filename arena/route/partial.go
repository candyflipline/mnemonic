package route

import (
	"context"
	"encoding/json"
)

// partial answers a call at rung Partial: scatter each phase, then merge
// exactly by the tool's merge rule.
func partial(ctx context.Context, v View, readers []Reader, tool string, args json.RawMessage) (json.RawMessage, string, error) {
	panic("lane L7")
}
