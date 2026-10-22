package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

type getIngestStatus struct{}

func (getIngestStatus) Name() string { return "get_ingest_status" }

func init() { register(getIngestStatus{}) }

func (getIngestStatus) Call(ctx context.Context, db *sql.DB, args json.RawMessage) (any, string, error) {
	if !stEmptyArgs(args) {
		return nil, "", argErrorf("get_ingest_status expects an empty JSON object")
	}
	read, err := stReadSources(ctx, db)
	if err != nil {
		return nil, "", err
	}
	status := stDerive(read, stUnixNow())
	data := stStatusJSON(status)
	return data, stStatusText(status), nil
}

func stEmptyArgs(args json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || fields == nil {
		return false
	}
	return len(fields) == 0
}

func stUnixNow() int64 { return time.Now().UnixMilli() }

type stGroupRow struct {
	author      string
	machine     string
	updatedMs   int64
	sourceCount int64
	totalBytes  int64
	parsedCount int64
	parsedBytes int64
}

func stReadSources(ctx context.Context, db *sql.DB) (stStatusRead, error) {
	f, err := Sessions(ctx, db)
	if err != nil {
		return stStatusRead{}, err
	}
	where := " WHERE " + and("harness <> "+sqlQuote(controlHarness), f.Authors("author"))
	rows, err := db.QueryContext(ctx,
		"SELECT author, machine_id, max(updated_ms), count(*), coalesce(sum(size), 0),"+
			" count(*) FILTER (WHERE events > 0), coalesce(sum(size) FILTER (WHERE events > 0), 0)"+
			" FROM sources"+where+" GROUP BY author, machine_id ORDER BY author, machine_id")
	if err != nil {
		return stStatusRead{}, err
	}
	defer rows.Close()
	groups := []stGroupRow{}
	for rows.Next() {
		var g stGroupRow
		if err := rows.Scan(&g.author, &g.machine, &g.updatedMs, &g.sourceCount, &g.totalBytes, &g.parsedCount, &g.parsedBytes); err != nil {
			return stStatusRead{}, err
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return stStatusRead{}, err
	}
	if len(groups) == 0 {
		return stStatusRead{}, nil
	}
	heartbeats := make([]stHeartbeat, 0, len(groups))
	for _, g := range groups {
		heartbeats = append(heartbeats, stGroupHeartbeat(g))
	}
	latestIdx := 0
	for i := range heartbeats {
		a, b := heartbeats[i], heartbeats[latestIdx]
		if a.TsUnixMs > b.TsUnixMs || (a.TsUnixMs == b.TsUnixMs && a.Host > b.Host) {
			latestIdx = i
		}
	}
	history := append([]stHeartbeat{}, heartbeats...)
	sort.SliceStable(history, func(i, j int) bool { return history[i].TsUnixMs < history[j].TsUnixMs })
	return stStatusRead{Latest: &heartbeats[latestIdx], History: history}, nil
}

func stGroupHeartbeat(g stGroupRow) stHeartbeat {
	return stHeartbeat{
		TsUnixMs:                  g.updatedMs,
		Host:                      g.author + "/" + g.machine,
		FilesWatched:              uint32(g.sourceCount),
		LastDurableProgressUnixMs: uint64(g.updatedMs),
		FilesTotal:                uint64(g.sourceCount),
		FilesCompleted:            uint64(g.parsedCount),
		BytesTotal:                uint64(g.totalBytes),
		BytesCompleted:            uint64(g.parsedBytes),
	}
}

type stStatusJSONShape struct {
	ObservedAtUnixMs int64                `json:"observed_at_unix_ms"`
	Current          *stCurrentJSON       `json:"current"`
	Conditions       []stConditionJSON    `json:"conditions"`
	Alerts           []stAlertJSON        `json:"alerts"`
	Rate             *stRateJSON          `json:"rate"`
	Eta              *stEtaJSON           `json:"eta"`
	History          []stHistoryPointJSON `json:"history"`
}

type stCurrentJSON struct {
	HeartbeatObservedAtUnixMs int64           `json:"heartbeat_observed_at_unix_ms"`
	QueueDepth                uint64          `json:"queue_depth"`
	FilesActive               uint32          `json:"files_active"`
	FilesWatched              uint32          `json:"files_watched"`
	Progress                  *stProgressJSON `json:"progress"`
}

type stProgressJSON struct {
	DiscoveryComplete         bool   `json:"discovery_complete"`
	QueueCapacity             uint64 `json:"queue_capacity"`
	SinkPendingRows           uint64 `json:"sink_pending_rows"`
	SinkPendingBytes          uint64 `json:"sink_pending_bytes"`
	SinkRetrying              bool   `json:"sink_retrying"`
	OldestPendingUnixMs       uint64 `json:"oldest_pending_unix_ms"`
	LastDurableProgressUnixMs uint64 `json:"last_durable_progress_unix_ms"`
	FilesTotal                uint64 `json:"files_total"`
	FilesCompleted            uint64 `json:"files_completed"`
	BytesTotal                uint64 `json:"bytes_total"`
	BytesCompleted            uint64 `json:"bytes_completed"`
}

type stConditionJSON struct {
	ConditionType string           `json:"condition_type"`
	State         stConditionState `json:"state"`
	Reason        string           `json:"reason"`
	ObservedAt    int64            `json:"observed_at_unix_ms"`
}

type stAlertJSON struct {
	Code       stAlertCode `json:"code"`
	ObservedAt int64       `json:"observed_at_unix_ms"`
}

type stRateJSON struct {
	BytesPerSecond float64 `json:"bytes_per_second"`
	SampleSeconds  uint64  `json:"sample_seconds"`
}

type stEtaJSON struct {
	Scope       string `json:"scope"`
	LowSeconds  uint64 `json:"low_seconds"`
	HighSeconds uint64 `json:"high_seconds"`
}

type stHistoryPointJSON struct {
	TsUnixMs          int64  `json:"ts_unix_ms"`
	QueueDepth        uint64 `json:"queue_depth"`
	FilesActive       uint32 `json:"files_active"`
	QueueCapacity     uint64 `json:"queue_capacity"`
	SinkPendingRows   uint64 `json:"sink_pending_rows"`
	SinkRetrying      bool   `json:"sink_retrying"`
	DiscoveryComplete bool   `json:"discovery_complete"`
	FilesTotal        uint64 `json:"files_total"`
	FilesCompleted    uint64 `json:"files_completed"`
	BytesTotal        uint64 `json:"bytes_total"`
	BytesCompleted    uint64 `json:"bytes_completed"`
}

func stStatusJSON(status stStatus) *stStatusJSONShape {
	out := &stStatusJSONShape{
		ObservedAtUnixMs: status.ObservedAt,
		Conditions:       make([]stConditionJSON, 0, len(status.Conditions)),
		Alerts:           make([]stAlertJSON, 0, len(status.Alerts)),
		History:          make([]stHistoryPointJSON, 0, len(status.History)),
	}
	if c := status.Current; c != nil {
		out.Current = &stCurrentJSON{
			HeartbeatObservedAtUnixMs: c.TsUnixMs,
			FilesWatched:              c.FilesWatched,
			Progress: &stProgressJSON{
				DiscoveryComplete:         true,
				LastDurableProgressUnixMs: c.LastDurableProgressUnixMs,
				FilesTotal:                c.FilesTotal,
				FilesCompleted:            c.FilesCompleted,
				BytesTotal:                c.BytesTotal,
				BytesCompleted:            c.BytesCompleted,
			},
		}
	}
	for _, c := range status.Conditions {
		out.Conditions = append(out.Conditions, stConditionJSON{
			ConditionType: string(c.Type), State: c.State, Reason: c.Reason, ObservedAt: c.ObservedAt,
		})
	}
	for _, a := range status.Alerts {
		out.Alerts = append(out.Alerts, stAlertJSON{Code: a.Code, ObservedAt: a.ObservedAt})
	}
	if status.Rate != nil {
		out.Rate = &stRateJSON{BytesPerSecond: status.Rate.BytesPerSecond, SampleSeconds: status.Rate.SampleSeconds}
	}
	if status.Eta != nil {
		out.Eta = &stEtaJSON{Scope: status.Eta.Scope, LowSeconds: status.Eta.LowSeconds, HighSeconds: status.Eta.HighSeconds}
	}
	for _, h := range status.History {
		out.History = append(out.History, stHistoryPointJSON{
			TsUnixMs: h.TsUnixMs, DiscoveryComplete: true,
			FilesTotal: h.FilesTotal, FilesCompleted: h.FilesCompleted,
			BytesTotal: h.BytesTotal, BytesCompleted: h.BytesCompleted,
		})
	}
	return out
}

func stConditionStateText(status stStatus, kind stConditionType) (string, string) {
	for _, c := range status.Conditions {
		if c.Type != kind {
			continue
		}
		state := "unknown"
		switch c.State {
		case stStateTrue:
			state = "ok"
		case stStateFalse:
			state = "degraded"
		}
		return state, c.Reason
	}
	return "unknown", "condition_missing"
}

func stStatusText(status stStatus) string {
	health, healthReason := stConditionStateText(status, stCondHealth)
	coverage, coverageReason := stConditionStateText(status, stCondCoverage)
	freshness, freshnessReason := stConditionStateText(status, stCondFreshness)
	readiness, readinessReason := stConditionStateText(status, stCondReadiness)
	lines := []string{sprintf(
		"Ingestion health=%s (%s); coverage=%s (%s); freshness=%s (%s); readiness=%s (%s).",
		health, healthReason, coverage, coverageReason, freshness, freshnessReason, readiness, readinessReason)}

	if progress := status.Current; progress != nil {
		var percent float64
		switch {
		case progress.BytesTotal > 0:
			percent = float64(progress.BytesCompleted) * 100.0 / float64(progress.BytesTotal)
		case progress.FilesTotal > 0:
			percent = float64(progress.FilesCompleted) * 100.0 / float64(progress.FilesTotal)
		default:
			percent = 100.0
		}
		lines = append(lines, sprintf(
			"Historical startup snapshot: %d/%d files, %d/%d bytes (%.1f%%).",
			progress.FilesCompleted, progress.FilesTotal, progress.BytesCompleted, progress.BytesTotal, percent))
	}
	if status.Rate != nil {
		lines = append(lines, sprintf("Durable checkpoint rate: %.0f bytes/s over %ds.",
			status.Rate.BytesPerSecond, status.Rate.SampleSeconds))
	}
	if status.Eta != nil {
		lines = append(lines, sprintf("Estimated %s completion: %d-%ds.",
			status.Eta.Scope, status.Eta.LowSeconds, status.Eta.HighSeconds))
	}
	if len(status.Alerts) > 0 {
		lines = append(lines, sprintf("Active alerts: %d.", len(status.Alerts)))
	}
	return strings.Join(lines, "\n")
}
