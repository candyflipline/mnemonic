package shared

// CanonicalJSON is payload JSON in the exact bytes Compact produces: object
// keys sorted, no whitespace, canonical numbers. The field is unexported, so
// only this package can build one — Compact does — and the semantic-uid and
// tool-fold fast paths take it, so payload bytes those paths did not produce
// cannot reach them.
type CanonicalJSON struct{ s string }

func (c CanonicalJSON) String() string { return c.s }

// CompactCanonical serializes v the way serde_json::to_string does and types
// the bytes as CanonicalJSON.
func CompactCanonical(v JSON) CanonicalJSON { return CanonicalJSON{s: Compact(v)} }
