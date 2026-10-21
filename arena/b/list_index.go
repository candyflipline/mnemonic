package main

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (db *Store) buildListRollups() {
	out := mappedSlice[lsItem](&db.arena, len(db.sessions))[:0]
	maxEvents := 0
	for _, es := range db.sessions {
		maxEvents = max(maxEvents, len(es))
	}
	scratch := mappedSlice[*event](&db.arena, maxEvents)
	for sid, es := range db.sessions {
		ss := db.stats[sid]
		if strings.TrimSpace(sid) == "" {
			continue
		}
		sorted := scratch[:len(es)]
		copy(sorted, es)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Timestamp != sorted[j].Timestamp {
				return sorted[i].Timestamp < sorted[j].Timestamp
			}
			return sorted[i].EventUID < sorted[j].EventUID
		})
		var source, harness, title, summary, explicit, slug, mt, mn, ms, preview, terminal string
		var terminalTurn, turns uint32
		var rank uint8
		for _, e := range sorted {
			if e.SourceName != "" {
				source = e.SourceName
			}
			if e.Harness != "" {
				harness = e.Harness
			}
			turns = max(turns, e.Turn)
			n := db.nav(e)
			rank = max(rank, eventModeRank(&n))
			if e.PayloadType == "task_complete" || e.PayloadType == "turn_aborted" {
				terminal = e.PayloadType
				terminalTurn = max(terminalTurn, e.TurnIndex)
			}
			if e.EventKind == "session_meta" {
				t, n, s, g := payloadString(e.PayloadJSON, "title"), payloadString(e.PayloadJSON, "name"), payloadString(e.PayloadJSON, "summary"), payloadString(e.PayloadJSON, "slug")
				if v := firstNonEmpty(t, n, s); v != "" {
					title = v
				}
				if v := firstNonEmpty(s, t, n); v != "" {
					summary = v
				}
				if strings.TrimSpace(t) != "" || strings.TrimSpace(n) != "" {
					explicit = firstNonEmpty(t, n)
				}
				if g != "" {
					slug = g
				}
				if t != "" {
					mt = t
				}
				if n != "" {
					mn = n
				}
				if s != "" {
					ms = s
				}
			} else if e.SourceName == "omp" {
				kind := payloadString(e.PayloadJSON, "type")
				if kind == "title" || kind == "title_change" {
					if t := payloadString(e.PayloadJSON, "title"); t != "" {
						mt = t
					}
				}
			}
		}
		// arg_min for the Codex preview uses source coordinates after timestamp.
		sort.Slice(sorted, func(i, j int) bool {
			x, y := sorted[i], sorted[j]
			if x.Timestamp != y.Timestamp {
				return x.Timestamp < y.Timestamp
			}
			if x.SourceName != y.SourceName {
				return x.SourceName < y.SourceName
			}
			xx, yy := *x, *y
			xx.Time = 0
			yy.Time = 0
			xx.Emission = 0
			yy.Emission = 0
			return eventLess(&xx, &yy)
		})
		dispatch := ""
		for _, e := range sorted {
			if preview == "" && e.Harness == "codex" && e.ActorKind == "user" && e.EventKind == "event_msg" && e.PayloadType == "user_message" {
				preview = strings.TrimSpace(truncateRunes(e.TextPreview, 320))
			}
			if dispatch == "" && e.SourceName == "omp" && strings.HasSuffix(e.SourceFile, ".jsonl") && !strings.HasSuffix(e.SourceFile, sid+".jsonl") {
				parts := strings.Split(strings.ReplaceAll(e.SourceFile, "\\", "/"), "/")
				dispatch = strings.TrimSpace(strings.TrimSuffix(parts[len(parts)-1], ".jsonl"))
			}
		}
		if source == "omp" {
			title = firstNonEmpty(mt, mn, ms, dispatch)
			summary = firstNonEmpty(ms, mt, mn, dispatch)
		}
		mode := modeFromRank(rank)
		if harness != "codex" {
			preview = ""
		}
		item := lsItem{SessionID: sid, FirstEventUnixMs: ss.Start, LastEventUnixMs: ss.End, FirstEventTime: qFormatDateTime(time.UnixMilli(ss.Start)), LastEventTime: qFormatDateTime(time.UnixMilli(ss.End)), TotalTurns: int(turns), TotalEvents: int64(len(es)), Mode: mode, Completed: terminalTurn == turns && terminal == "task_complete", Title: strPtr(title), Source: strPtr(source), Harness: strPtr(harness), SessionSlug: strPtr(slug), SessionSummary: strPtr(summary)}
		item.DisplayLabel = lsDisplayLabel(explicit, preview, title, summary, slug, harness, lsParseMode(mode), item.LastEventTime, item.TotalTurns)
		for _, p := range []*string{&item.SessionID, &item.FirstEventTime, &item.LastEventTime, &item.Mode, &item.DisplayLabel} {
			*p = db.arena.text(*p)
		}
		for _, p := range []**string{&item.Title, &item.Source, &item.Harness, &item.SessionSlug, &item.SessionSummary} {
			if *p != nil {
				v := &mappedSlice[string](&db.arena, 1)[0]
				*v = db.arena.text(**p)
				*p = v
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.LastEventUnixMs != y.LastEventUnixMs {
			return x.LastEventUnixMs < y.LastEventUnixMs
		}
		return x.SessionID < y.SessionID
	})
	db.listRows = out
}

func lsSelectPage(_ context.Context, db *Store, a *lsArgs, cursor *lsCursor) ([]lsItem, error) {
	rows := db.listRows
	start, step := 0, 1
	if a.Sort == lsSortDesc {
		start = len(rows) - 1
		step = -1
	}
	if cursor != nil {
		if step == 1 {
			start = sort.Search(len(rows), func(i int) bool {
				r := rows[i]
				return r.LastEventUnixMs > cursor.LastEventUnixMs || r.LastEventUnixMs == cursor.LastEventUnixMs && r.SessionID > cursor.SessionID
			})
		} else {
			start = sort.Search(len(rows), func(i int) bool {
				r := rows[i]
				return r.LastEventUnixMs > cursor.LastEventUnixMs || r.LastEventUnixMs == cursor.LastEventUnixMs && r.SessionID >= cursor.SessionID
			}) - 1
		}
	}
	var out []lsItem
	for i := start; i >= 0 && i < len(rows); i += step {
		r := rows[i]
		if r.LastEventUnixMs < a.StartUnixMs || r.FirstEventUnixMs >= a.EndUnixMs {
			continue
		}
		if a.Mode != nil && r.Mode != *a.Mode || a.Harness != nil && (r.Harness == nil || *r.Harness != *a.Harness) || a.Source != nil && (r.Source == nil || *r.Source != *a.Source) {
			continue
		}
		out = append(out, r)
		if len(out) == a.Limit+1 {
			break
		}
	}
	return out, nil
}
