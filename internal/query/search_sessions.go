package query

// search_sessions: BM25 keyword search over the cache, ported from upstream
// crates/moraine-conversations/src/clickhouse_repo/search.rs (the MCP event
// search path) and crates/moraine-mcp-core/src/search_sessions_v1.rs (args,
// data shape, prose). Owned by lane cache.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type searchSessionsTool struct{}

func (searchSessionsTool) Name() string { return "search_sessions" }

func init() { register(searchSessionsTool{}) }

// canonicalSearchSessionsArgs is contract.rs CanonicalSearchSessionsArgs.
type canonicalSearchSessionsArgs struct {
	query      string
	withinID   *mcpID
	eventTypes []string
	harness    *string
	source     *string
	nHits      int
}

// parseSearchSessionsArgs ports SearchSessionsArgs::validate; every message is
// the upstream contract text.
func parseSearchSessionsArgs(raw json.RawMessage) (canonicalSearchSessionsArgs, error) {
	var args struct {
		Query      *string           `json:"query"`
		WithinID   *string           `json:"within_id"`
		EventTypes []string          `json:"event_types"`
		Harness    *string           `json:"harness"`
		Source     *string           `json:"source"`
		NHits      *int              `json:"n_hits"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return canonicalSearchSessionsArgs{}, argErrorf(
			"search_sessions expects a JSON object with a string query")
	}
	if args.Query == nil {
		return canonicalSearchSessionsArgs{}, argErrorf(
			"search_sessions expects a JSON object with a string query")
	}

	a := canonicalSearchSessionsArgs{}
	a.query = strings.TrimSpace(*args.Query)
	if a.query == "" {
		return canonicalSearchSessionsArgs{}, argErrorf("query must be a non-empty string")
	}
	if len([]rune(a.query)) > searchMaxQueryChars {
		return canonicalSearchSessionsArgs{}, argErrorf("query must be at most 4096 characters")
	}

	if args.WithinID != nil {
		id, err := parseMcpID(*args.WithinID)
		if err != nil {
			return canonicalSearchSessionsArgs{}, err
		}
		if id.kind == kindEvent {
			return canonicalSearchSessionsArgs{}, argErrorf(
				"within_id accepts session and turn IDs, not event IDs")
		}
		a.withinID = &id
	}

	var err error
	if a.eventTypes, err = canonicalizeSearchEventTypeNames(args.EventTypes); err != nil {
		return canonicalSearchSessionsArgs{}, err
	}
	if a.harness, err = normalizeOptionalFilter("harness", args.Harness); err != nil {
		return canonicalSearchSessionsArgs{}, err
	}
	if a.source, err = normalizeOptionalFilter("source", args.Source); err != nil {
		return canonicalSearchSessionsArgs{}, err
	}

	a.nHits = 10
	if args.NHits != nil {
		a.nHits = *args.NHits
	}
	if a.nHits < 1 || a.nHits > 50 {
		return canonicalSearchSessionsArgs{}, argErrorf("n_hits must be between 1 and 50")
	}
	return a, nil
}

// normalizeOptionalFilter ports contract.rs normalize_optional_filter.
func normalizeOptionalFilter(field string, value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil, argErrorf("%s must be a non-empty string when provided", field)
	}
	return &trimmed, nil
}

// requestShape is the canonical request the envelope echoes: serde serializes
// absent options as null, so every key is present.
func (a canonicalSearchSessionsArgs) requestShape() map[string]any {
	var within any
	if a.withinID != nil {
		within = a.withinID.String()
	}
	return map[string]any{
		"query":       a.query,
		"within_id":   within,
		"event_types": a.eventTypes,
		"harness":     derefOrNil(a.harness),
		"source":      derefOrNil(a.source),
		"n_hits":      a.nHits,
	}
}

func derefOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func (searchSessionsTool) Call(ctx context.Context, db *sql.DB, raw json.RawMessage) (any, string, error) {
	args, err := parseSearchSessionsArgs(raw)
	if err != nil {
		return nil, "", err
	}
	request := args.requestShape()

	result, err := searchMcpEvents(ctx, db, searchQuery{
		query:      args.query,
		nHits:      args.nHits,
		sessionID:  scopeSessionID(args.withinID),
		turnSeq:    scopeTurnSeq(args.withinID),
		eventTypes: args.eventTypes,
		harness:    args.harness,
		sourceName: args.source,
	})
	if err != nil {
		return nil, "", err
	}

	// Upstream search_sessions_v1: a missing scope is a not-found error, not
	// an empty result.
	if !result.scopeExists {
		switch {
		case args.withinID == nil:
			return nil, "", argErrorf("repository reported a missing scope for an unscoped search")
		case args.withinID.kind == kindSession:
			return nil, "", argErrorf("session not found")
		default:
			return nil, "", argErrorf("turn not found")
		}
	}

	results := make([]map[string]any, 0, len(result.hits))
	for _, hit := range result.hits {
		results = append(results, searchHitJSON(hit))
	}
	data := map[string]any{
		"result_count": len(results),
		"limit":        args.nHits,
		"truncated":    result.truncated,
		"results":      results,
	}
	text := formatSearchSessionsText(request, data)
	return data, text, nil
}

func scopeSessionID(id *mcpID) (string, bool) {
	if id == nil {
		return "", false
	}
	return id.session, true
}

func scopeTurnSeq(id *mcpID) (uint32, bool) {
	if id == nil || id.kind != kindTurn {
		return 0, false
	}
	return id.turnSeq, true
}

// searchHitJSON ports search_sessions_v1.rs search_hit_json.
func searchHitJSON(hit searchHit) map[string]any {
	eventID := formatEventID(hit.eventUID)
	turnID := formatTurnID(hit.sessionID, hit.turnSeq)
	sessionID := formatSessionID(hit.sessionID)
	snippet, snippetTruncated := compactSnippet(hit.snippet, hit.snippetTruncated)

	title := firstNonEmpty(hit.sessionTitle, hit.sessionSummary, hit.sessionSlug)
	var startedAt, updatedAt any
	if hit.sessionStartedAtUnixMs != 0 {
		startedAt = formatRFC3339UTCMillis(hit.sessionStartedAtUnixMs)
	}
	if hit.sessionUpdatedAtUnixMs != 0 {
		updatedAt = formatRFC3339UTCMillis(hit.sessionUpdatedAtUnixMs)
	}
	return map[string]any{
		"rank":  hit.rank,
		"score": scoreUnit(hit.score),
		"id":    eventID,
		"event": map[string]any{
			"id":        eventID,
			"type":      hit.eventType,
			"timestamp": formatRFC3339UTCMillis(hit.eventUnixMs),
			"ordinal":   hit.eventOrdinal,
			"terminal":  hit.turnTerminalEventUID == hit.eventUID,
		},
		"turn": map[string]any{
			"id":          turnID,
			"ordinal":     hit.turnOrdinal,
			"completed":   hit.turnCompleted,
			"event_count": hit.turnEventCount,
		},
		"session": map[string]any{
			"id":         sessionID,
			"title":      nonEmpty(title),
			"harness":    nonEmpty(hit.harness),
			"source":     nonEmpty(firstNonEmpty(hit.sourceName, hit.harness)),
			"started_at": startedAt,
			"updated_at": updatedAt,
			"completed":  hit.sessionCompleted,
		},
		"snippet": map[string]any{
			"text":      snippet,
			"truncated": snippetTruncated,
		},
		"open": map[string]any{
			"event_id":   eventID,
			"turn_id":    turnID,
			"session_id": sessionID,
		},
	}
}

// compactSnippet ports search_sessions_v1.rs compact_snippet.
func compactSnippet(snippet string, alreadyTruncated bool) (string, bool) {
	trimmed := strings.TrimSpace(snippet)
	if len([]rune(trimmed)) <= maxSnippetChars {
		return trimmed, alreadyTruncated
	}
	compact := string([]rune(trimmed)[:maxSnippetChars-3]) + "..."
	return compact, true
}

// scoreUnit ports search_sessions_v1.rs score_unit.
func scoreUnit(score float64) float64 {
	if score != score || score > 1.7976931348623157e308 || score < -1.7976931348623157e308 {
		return 0
	}
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

// formatSearchSessionsText ports format_search_sessions_text: it reads the
// request and data halves of the envelope, so it takes them separately.
func formatSearchSessionsText(request, data map[string]any) string {
	query, _ := request["query"].(string)
	resultCount := 0
	if n, ok := data["result_count"].(int); ok {
		resultCount = n
	}
	lines := []string{fmt.Sprintf("Found %d search hit(s) for %q.", resultCount, query)}

	results, _ := data["results"].([]map[string]any)
	for i, result := range results {
		if i >= 10 {
			break
		}
		rank := jsonAsInt64(result["rank"])
		score := jsonAsFloat64(result["score"])
		eventType := "event"
		if event, ok := result["event"].(map[string]any); ok {
			if t, ok := event["type"].(string); ok && t != "" {
				eventType = t
			}
		}
		eventID := ""
		if open, ok := result["open"].(map[string]any); ok {
			if id, ok := open["event_id"].(string); ok {
				eventID = id
			}
		}
		title := "untitled session"
		if session, ok := result["session"].(map[string]any); ok {
			if t, ok := session["title"].(string); ok && t != "" {
				title = t
			}
		}
		snippet := ""
		if sn, ok := result["snippet"].(map[string]any); ok {
			if t, ok := sn["text"].(string); ok {
				snippet = t
			}
		}
		lines = append(lines, fmt.Sprintf("%d. %s score=%.2f %s: %s (%s)",
			rank, eventType, score, title, snippet, eventID))
	}
	return strings.Join(lines, "\n")
}
