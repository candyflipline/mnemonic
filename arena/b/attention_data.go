package main

import (
	"sort"
	"strings"
)

const faUnknownRoot = "(unknown)"

// faBuildData: warnings are appended in the upstream order.
func faBuildData(a *faArgs, tail *faTail, touches []faTouch, warnings *[]string) map[string]any {
	scanTruncated := len(touches) > faScanCap
	if scanTruncated {
		touches = touches[:faScanCap]
		*warnings = append(*warnings, sprintf(
			"matched more than %d touches; summary and roots are computed over the %d most recent. Narrow with start_datetime/end_datetime or tool.",
			faScanCap, faScanCap))
	}

	valid := make([]faTouch, 0, len(touches))
	skipped := 0
	for _, touch := range touches {
		if _, err := qTypedID("session:", touch.SessionID); err != nil {
			skipped++
			continue
		}
		if _, err := qTypedID("event:", touch.EventUID); err != nil {
			skipped++
			continue
		}
		valid = append(valid, touch)
	}
	touches = valid
	if skipped > 0 {
		*warnings = append(*warnings, sprintf(
			"dropped %d touch(es) whose repository identifiers could not be encoded; they are excluded from the result and its counts.", skipped))
	}

	totalTouches := len(touches)
	var firstMs, lastMs *int64
	distinctSessions := map[string]bool{}
	type rootStat struct {
		touches  int
		sessions map[string]bool
	}
	rootStats := map[string]*rootStat{}
	for i := range touches {
		touch := &touches[i]
		if touch.EventUnixMs != nil {
			if firstMs == nil || *touch.EventUnixMs < *firstMs {
				v := *touch.EventUnixMs
				firstMs = &v
			}
			if lastMs == nil || *touch.EventUnixMs > *lastMs {
				v := *touch.EventUnixMs
				lastMs = &v
			}
		}
		distinctSessions[touch.SessionID] = true
		key := faRootLabel(touch.WorktreeRoot)
		entry := rootStats[key]
		if entry == nil {
			entry = &rootStat{sessions: map[string]bool{}}
			rootStats[key] = entry
		}
		entry.touches++
		entry.sessions[touch.SessionID] = true
	}

	distinctKnownRoots := 0
	for root := range rootStats {
		if root != faUnknownRoot {
			distinctKnownRoots++
		}
	}
	unknownRootTouches := 0
	if entry := rootStats[faUnknownRoot]; entry != nil {
		unknownRootTouches = entry.touches
	}
	ambiguous := len(rootStats) > 1 || (unknownRootTouches > 0 && totalTouches > 1)
	if unknownRootTouches > 0 {
		*warnings = append(*warnings, sprintf(
			"%d touch(es) have unknown worktree roots; check matched_path/open results before treating the root spread as complete.",
			unknownRootTouches))
	}

	roots := make([]any, 0, len(rootStats))
	for root, stat := range rootStats {
		roots = append(roots, map[string]any{
			"root":          root,
			"touch_count":   stat.touches,
			"session_count": len(stat.sessions),
		})
	}
	sort.Slice(roots, func(i, j int) bool {
		a, b := roots[i].(map[string]any), roots[j].(map[string]any)
		if intAt(a, "touch_count") != intAt(b, "touch_count") {
			return intAt(a, "touch_count") > intAt(b, "touch_count")
		}
		return stringAt(a, "root") < stringAt(b, "root")
	})

	summary := map[string]any{
		"total_touches":        totalTouches,
		"distinct_sessions":    len(distinctSessions),
		"distinct_roots":       len(rootStats),
		"distinct_known_roots": distinctKnownRoots,
		"unknown_root_touches": unknownRootTouches,
		"first_touch":          faFormatMillisPtr(firstMs),
		"last_touch":           faFormatMillisPtr(lastMs),
		"ambiguous":            ambiguous,
		"scan_truncated":       scanTruncated,
	}

	limit := a.Limit
	var bodyKey string
	var body []any
	var encodable, rollupSkipped int
	if a.Granularity == faGranularityEvents {
		bodyKey = "events"
		body, encodable, rollupSkipped = faEventTimeline(touches, limit)
	} else {
		bodyKey = "sessions"
		body, encodable, rollupSkipped = faSessionRollups(touches, limit)
	}
	if rollupSkipped > 0 {
		*warnings = append(*warnings, sprintf(
			"dropped %d touch(es) whose repository identifiers could not be encoded; they are excluded from the result and its counts.", rollupSkipped))
	}

	data := map[string]any{
		"path":             a.Path,
		"tail":             tail.Rel,
		"tail_is_absolute": strings.HasPrefix(tail.Rel, "/"),
		"stripped_root":    nil,
		"scope":            string(a.Scope),
		"granularity":      string(a.Granularity),
		"summary":          summary,
		"roots":            roots,
		"result_count":     len(body),
		"limit":            limit,
		"truncated":        encodable > limit || scanTruncated,
	}
	if tail.Root != "" {
		data["stripped_root"] = tail.Root
	}
	data[bodyKey] = body
	return data
}

func faRootLabel(root string) string {
	if root == "" {
		return faUnknownRoot
	}
	return root
}

func faFormatMillisPtr(ms *int64) any {
	if ms == nil {
		return nil
	}
	return formatRFC3339UTCMillis(*ms)
}

func faSortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for v := range values {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

type faSessionAgg struct {
	firstMs, lastMs *int64
	touchCount      int
	tools           map[string]bool
	roots           map[string]bool
	matchKinds      map[string]bool
	harness         string
	source          string
	latestEventUID  string
	latestTurnSeq   *uint32
}

func faSessionRollups(touches []faTouch, limit int) ([]any, int, int) {
	order := []string{}
	aggs := map[string]*faSessionAgg{}
	for i := range touches {
		touch := &touches[i]
		agg := aggs[touch.SessionID]
		if agg == nil {
			order = append(order, touch.SessionID)
			agg = &faSessionAgg{
				tools:          map[string]bool{},
				roots:          map[string]bool{},
				matchKinds:     map[string]bool{},
				harness:        touch.Harness,
				source:         touch.SourceName,
				latestEventUID: touch.EventUID,
				latestTurnSeq:  touch.TurnSeq,
			}
			aggs[touch.SessionID] = agg
		}
		agg.touchCount++
		if touch.ToolName != "" {
			agg.tools[touch.ToolName] = true
		}
		if touch.WorktreeRoot != "" {
			agg.roots[touch.WorktreeRoot] = true
		}
		if touch.MatchKind != "" {
			agg.matchKinds[touch.MatchKind] = true
		}
		if agg.harness == "" {
			agg.harness = touch.Harness
		}
		if agg.source == "" {
			agg.source = touch.SourceName
		}
		if touch.EventUnixMs != nil {
			if agg.firstMs == nil || *touch.EventUnixMs < *agg.firstMs {
				v := *touch.EventUnixMs
				agg.firstMs = &v
			}
			if agg.lastMs == nil || *touch.EventUnixMs > *agg.lastMs {
				v := *touch.EventUnixMs
				agg.lastMs = &v
			}
		}
	}

	sessions := []any{}
	encodable, skipped := 0, 0
	for _, sessionID := range order {
		agg := aggs[sessionID]
		mcpSessionID, err := qTypedID("session:", sessionID)
		if err != nil {
			skipped++
			continue
		}
		latestEventID, err := qTypedID("event:", agg.latestEventUID)
		if err != nil {
			skipped++
			continue
		}
		encodable++
		if len(sessions) >= limit {
			continue
		}
		open := map[string]any{
			"session_id": mcpSessionID,
			"event_id":   latestEventID,
		}
		if agg.latestTurnSeq != nil {
			open["turn_id"] = formatTurnID(sessionID, *agg.latestTurnSeq)
		}
		sessions = append(sessions, map[string]any{
			"rank": len(sessions) + 1,
			"id":   mcpSessionID,
			"session": map[string]any{
				"id":             mcpSessionID,
				"harness":        agg.harness,
				"source":         agg.source,
				"first_touch":    faFormatMillisPtr(agg.firstMs),
				"last_touch":     faFormatMillisPtr(agg.lastMs),
				"touch_count":    agg.touchCount,
				"tools":          faSortedSet(agg.tools),
				"worktree_roots": faSortedSet(agg.roots),
				"match_kinds":    faSortedSet(agg.matchKinds),
			},
			"open": open,
		})
	}
	return sessions, encodable, skipped
}

func faEventTimeline(touches []faTouch, limit int) ([]any, int, int) {
	events := []any{}
	encodable, skipped := 0, 0
	for i := range touches {
		touch := &touches[i]
		eventID, err := qTypedID("event:", touch.EventUID)
		if err != nil {
			skipped++
			continue
		}
		sessionID, err := qTypedID("session:", touch.SessionID)
		if err != nil {
			skipped++
			continue
		}
		encodable++
		if len(events) >= limit {
			continue
		}
		open := map[string]any{
			"event_id":   eventID,
			"session_id": sessionID,
		}
		if touch.TurnSeq != nil {
			open["turn_id"] = formatTurnID(touch.SessionID, *touch.TurnSeq)
		}
		var worktreeRoot any
		if touch.WorktreeRoot != "" {
			worktreeRoot = touch.WorktreeRoot
		}
		events = append(events, map[string]any{
			"rank": len(events) + 1,
			"id":   eventID,
			"event": map[string]any{
				"id":             eventID,
				"session_id":     sessionID,
				"timestamp":      faFormatMillisPtr(touch.EventUnixMs),
				"harness":        touch.Harness,
				"source":         touch.SourceName,
				"tool_name":      touch.ToolName,
				"phase":          touch.ToolPhase,
				"turn":           faUint32Ptr(touch.TurnSeq),
				"match_kind":     touch.MatchKind,
				"worktree_root":  worktreeRoot,
				"action_preview": faActionPreview(touch),
			},
			"open": open,
		})
	}
	return events, encodable, skipped
}

func faUint32Ptr(v *uint32) any {
	if v == nil {
		return nil
	}
	return *v
}

// faActionPreview draws a bounded preview from the canonical event's tool
// input/output previews; the cap is chars, not bytes.
func faActionPreview(touch *faTouch) any {
	raw := touch.InputPreview
	if raw == "" {
		raw = touch.OutputPreview
	}
	if raw == "" {
		return nil
	}
	collapsed := strings.Join(strings.Fields(raw), " ")
	const max = 160
	runes := []rune(collapsed)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return collapsed
}

func faText(payload map[string]any) string {
	data, _ := payload["data"].(map[string]any)
	tail := stringAt(data, "tail")
	scope := stringAt(data, "scope")
	if scope == "" {
		scope = "project"
	}
	granularity := stringAt(data, "granularity")
	if granularity == "" {
		granularity = "sessions"
	}
	summary, _ := data["summary"].(map[string]any)
	total := intAt(summary, "total_touches")
	sessions := intAt(summary, "distinct_sessions")
	roots := intAt(summary, "distinct_roots")

	lines := []string{sprintf(
		"file_attention %s — %d touch(es) across %d session(s) in %d worktree root(s) [scope=%s].",
		tail, total, sessions, roots, scope)}

	distinctKnownRoots, haveKnownRoots := intAtOK(summary, "distinct_known_roots")
	if !haveKnownRoots {
		distinctKnownRoots = roots
	}
	unknownRootTouches := intAt(summary, "unknown_root_touches")
	if distinctKnownRoots > 1 {
		lines = append(lines, "Ambiguous tail: matched more than one worktree root (see roots).")
	}
	if unknownRootTouches > 0 {
		lines = append(lines,
			"Worktree-root attribution is incomplete; some touches have unknown roots (see roots and matched_path).")
	}
	if rawWarnings, ok := payload["warnings"].([]string); ok {
		for _, warning := range rawWarnings {
			lines = append(lines, "⚠ "+warning)
		}
	}

	if granularity == "events" {
		if events, ok := data["events"].([]any); ok {
			for i, e := range events {
				if i >= 10 {
					break
				}
				event, _ := e.(map[string]any)
				inner, _ := event["event"].(map[string]any)
				open, _ := event["open"].(map[string]any)
				ts := stringAt(inner, "timestamp")
				if ts == "" {
					ts = "?"
				}
				lines = append(lines, sprintf("%d. %s %s (%s)",
					intAt(event, "rank"), ts, stringAt(inner, "tool_name"), stringAt(open, "event_id")))
			}
		}
	} else if ss, ok := data["sessions"].([]any); ok {
		for i, s := range ss {
			if i >= 10 {
				break
			}
			session, _ := s.(map[string]any)
			inner, _ := session["session"].(map[string]any)
			open, _ := session["open"].(map[string]any)
			last := stringAt(inner, "last_touch")
			if last == "" {
				last = "?"
			}
			lines = append(lines, sprintf("%d. %s %s %d touch(es) (%s)",
				intAt(session, "rank"), last, stringAt(inner, "harness"),
				intAt(inner, "touch_count"), stringAt(open, "session_id")))
		}
	}

	truncated, _ := data["truncated"].(bool)
	if truncated {
		lines = append(lines, "More results were truncated by limit.")
	}
	return strings.Join(lines, "\n")
}

func faFormatErrorText(code, message string) string {
	if code == "" {
		code = "internal_error"
	}
	if message == "" {
		message = "file_attention failed"
	}
	return sprintf("file_attention error (%s): %s", code, message)
}
