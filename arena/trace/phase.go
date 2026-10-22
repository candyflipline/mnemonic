package trace

import "encoding/json"

// RunPhase answers one phase of a tool scattered over the parts of db; stats
// carries what an earlier phase gathered across every reader.
func RunPhase(db *DB, tool, phase string, args, stats json.RawMessage) (json.RawMessage, error) {
	panic("lane L7")
}
