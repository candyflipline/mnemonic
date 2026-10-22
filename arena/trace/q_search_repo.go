package trace

import (
	"slices"
	"sort"
	"strings"
)

type searchQuery struct {
	query      string
	nHits      int
	sessionID  string
	sessionOK  bool
	turnSeq    uint32
	turnOK     bool
	eventTypes []string
	harness    *string
	sourceName *string
}

type searchHit struct {
	rank                   int
	eventUID               string
	sessionID              string
	eventType              string
	eventUnixMs            int64
	turnSeq                uint32
	turnOrdinal            uint32
	eventOrder             int64
	eventOrdinal           uint32
	turnEventCount         int64
	turnCompleted          bool
	turnTerminalEventUID   string
	sessionStartedAtUnixMs int64
	sessionUpdatedAtUnixMs int64
	sessionTitle           string
	sessionSummary         string
	sessionSlug            string
	sessionCompleted       bool
	sourceName             string
	harness                string
	inferenceProvider      string
	snippet                string
	snippetTruncated       bool
	score                  float64
	rawScore               float64
}

type searchResult struct {
	hits        []searchHit
	truncated   bool
	scopeExists bool
}

type mcpRow struct {
	eventUID    string
	sessionID   string
	doc         int64
	rawScore    float64
	eventTsMs   int64
	eventUnixMs int64
	mcpDetail
}

type mcpDetail struct {
	sourceName        string
	harness           string
	inferenceProvider string
	eventClass        string
	payloadType       string
	actorRole         string
	phase             string
	sourceRef         string
	textPreview       string
	textContent       string
	textHash          uint64
	eventOrder        int64
	turnSeq           uint32
	eventOrdinal      uint32
	turnEventCount    int64
	turnCompleted     bool
	turnTerminalUID   string
	sessionStartedMs  int64
	sessionUpdatedMs  int64
	sessionCompleted  bool
}

const flagSearchable = 1 << 8

func eventTypeMask(types []string) int {
	mask := 0
	for i, t := range searchableEventTypes {
		if slices.Contains(types, t) {
			mask |= 1 << i
		}
	}
	return mask
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func sortMcpRows(rows []*mcpRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.rawScore != b.rawScore {
			return a.rawScore > b.rawScore
		}
		if a.eventUnixMs != b.eventUnixMs {
			return a.eventUnixMs > b.eventUnixMs
		}
		return a.eventUID < b.eventUID
	})
}

func dedupeMcpRows(rows []*mcpRow, limit int) []*mcpRow {
	var out []*mcpRow
	for _, row := range rows {
		dup := false
		for _, existing := range out {
			if mcpRowsAreEquivalent(existing, row) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// mcpRowsAreEquivalent: same session, turn, event type and content. The
// codex-mirror extension (adjacent agent_message/message rows within 10 ms)
// rides on top, as upstream.
func mcpRowsAreEquivalent(a, b *mcpRow) bool {
	sameContent := a.textHash == b.textHash && a.textContent == b.textContent
	sameCoords := a.sessionID == b.sessionID && a.turnSeq == b.turnSeq &&
		eventTypeFor(a.eventClass, a.payloadType, a.actorRole) == eventTypeFor(b.eventClass, b.payloadType, b.actorRole) &&
		sameContent
	if !sameCoords {
		return false
	}
	return a.eventUnixMs == b.eventUnixMs || codexFinalAnswerMirrors(a, b)
}

const codexMirrorMaxDeltaMs = 10

func codexFinalAnswerMirrors(a, b *mcpRow) bool {
	pair := func(x, y *mcpRow) bool {
		return (x.eventClass == "message" && x.payloadType == "message" &&
			y.eventClass == "event_msg" && y.payloadType == "agent_message") ||
			(x.eventClass == "event_msg" && x.payloadType == "agent_message" &&
				y.eventClass == "message" && y.payloadType == "message")
	}
	isFinalAnswer := func(r *mcpRow) bool { return r.phase == "final_answer" }
	aRef, aOK := parseMcpSourceRef(a.sourceRef)
	bRef, bOK := parseMcpSourceRef(b.sourceRef)
	if !aOK || !bOK {
		return false
	}
	delta := a.eventUnixMs - b.eventUnixMs
	if delta < 0 {
		delta = -delta
	}
	orderDelta := a.eventOrder - b.eventOrder
	if orderDelta < 0 {
		orderDelta = -orderDelta
	}
	lineDelta := aRef.line - bRef.line
	if aRef.line < bRef.line {
		lineDelta = bRef.line - aRef.line
	}
	return a.harness == "codex" && b.harness == "codex" &&
		a.sourceName != "" && a.sourceName == b.sourceName &&
		pair(a, b) && isFinalAnswer(a) && isFinalAnswer(b) &&
		aRef.file == bRef.file && aRef.generation == bRef.generation &&
		lineDelta == 1 && orderDelta == 1 && delta <= codexMirrorMaxDeltaMs
}

type sourceRefParts struct {
	file       string
	generation uint64
	line       uint64
}

func parseMcpSourceRef(s string) (sourceRefParts, bool) {
	fileAndGen, line, ok1 := strings.Cut(s, ":")
	var parts sourceRefParts
	file, gen, ok2 := func() (string, string, bool) {
		i := strings.LastIndex(fileAndGen, ":")
		if i < 0 {
			return "", "", false
		}
		return fileAndGen[:i], fileAndGen[i+1:], true
	}()
	if !ok1 || !ok2 || file == "" || !allDigits(line) || !allDigits(gen) {
		return parts, false
	}
	parts.file = file
	parts.generation = parseUint(gen)
	parts.line = parseUint(line)
	return parts, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseUint(s string) uint64 {
	var n uint64
	for _, c := range s {
		n = n*10 + uint64(c-'0')
	}
	return n
}

func mapMcpRowsToHits(rows []*mcpRow) []searchHit {
	maxRaw := 0.0
	for _, r := range rows {
		if r.rawScore > maxRaw {
			maxRaw = r.rawScore
		}
	}
	hits := make([]searchHit, 0, len(rows))
	for i, r := range rows {
		score := 0.0
		if maxRaw > 0 {
			score = r.rawScore / maxRaw
			if score < 0 {
				score = 0
			}
			if score > 1 {
				score = 1
			}
		}
		snippet := r.textPreview
		snippetTruncated := len([]rune(r.textContent)) > len([]rune(r.textPreview)) && len([]rune(r.textPreview)) > 0
		if r.textPreview == "" {
			snippet = r.textContent
			snippetTruncated = false
		}
		hits = append(hits, searchHit{
			rank:                   i + 1,
			eventUID:               r.eventUID,
			sessionID:              r.sessionID,
			eventType:              eventTypeFor(r.eventClass, r.payloadType, r.actorRole),
			eventUnixMs:            r.eventUnixMs,
			turnSeq:                r.turnSeq,
			turnOrdinal:            r.turnSeq,
			eventOrder:             r.eventOrder,
			eventOrdinal:           r.eventOrdinal,
			turnEventCount:         r.turnEventCount,
			turnCompleted:          r.turnCompleted,
			turnTerminalEventUID:   r.turnTerminalUID,
			sessionStartedAtUnixMs: r.sessionStartedMs,
			sessionUpdatedAtUnixMs: r.sessionUpdatedMs,
			sessionCompleted:       r.sessionCompleted,
			sourceName:             r.sourceName,
			harness:                r.harness,
			inferenceProvider:      r.inferenceProvider,
			snippet:                snippet,
			snippetTruncated:       snippetTruncated,
			score:                  score,
			rawScore:               r.rawScore,
		})
	}
	return hits
}
