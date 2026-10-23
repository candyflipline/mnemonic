package redact

// Builtin rule 25 (generic-api-key), regex-backed. Lane U2d replaces this
// entry with a hand-written matcher meeting the matcher contract (see
// matchers.go). This is the hot rule: on the Oct 23 Mac measurement the
// generic regex is about 70% of redact time.

var m25 = regexEntry(25) // generic-api-key
