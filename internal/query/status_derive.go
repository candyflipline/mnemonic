package query

import (
	"math"
	"sort"
)

// The ingest-status domain, ported from the upstream conversations domain.rs. The
// read side (stStatusRead) carries raw heartbeats that must never serialize;
// only the projection in get_ingest_status.go reaches the wire.

type stConditionType string

const (
	stCondHealth    stConditionType = "health"
	stCondCoverage  stConditionType = "coverage"
	stCondFreshness stConditionType = "freshness"
	stCondReadiness stConditionType = "readiness"
)

type stConditionState string

const (
	stStateTrue    stConditionState = "true"
	stStateFalse   stConditionState = "false"
	stStateUnknown stConditionState = "unknown"
)

type stAlertCode string

const (
	stAlertHeartbeatStale  stAlertCode = "heartbeat_stale"
	stAlertProgressStalled stAlertCode = "progress_stalled"
	stAlertQueueSaturated  stAlertCode = "queue_saturated"
	stAlertSinkRetrying    stAlertCode = "sink_retrying"
	stAlertCoverageDegrade stAlertCode = "coverage_degraded"
)

// stHeartbeat ports IngestHeartbeat: the raw row. On this server the fields
// without a sources-table counterpart stay zero-valued.
type stHeartbeat struct {
	Ts                     string
	TsUnixMs               int64
	Host                   string
	ServiceVersion         string
	QueueDepth             uint64
	FilesActive            uint32
	FilesWatched           uint32
	RowsRawWritten         uint64
	RowsEventsWritten      uint64
	RowsErrorsWritten      uint64
	FlushLatencyMs         uint32
	AppendToVisibleP50Ms   uint32
	AppendToVisibleP95Ms   uint32
	LastError              string
	WatcherBackend         *string
	WatcherErrorCount      *uint64
	WatcherResetCount      *uint64
	WatcherLastResetUnixMs *uint64
	BackendSinks           string // raw JSON or ""
	Progress               *stProgress
}

type stProgress struct {
	SchemaVersion             uint16
	InstanceID                string
	RunStartedUnixMs          uint64
	SnapshotUnixMs            uint64
	DiscoveryComplete         bool
	QueueCapacity             uint64
	SinkPendingRows           uint64
	SinkPendingBytes          uint64
	SinkRetrying              bool
	OldestPendingUnixMs       uint64
	LastDurableProgressUnixMs uint64
	FilesTotal                uint64
	FilesCompleted            uint64
	BytesTotal                uint64
	BytesCompleted            uint64
	Sources                   []stSourceProgress
}

type stSourceProgress struct {
	SourceName       string
	Format           string
	CoverageBasis    string // "bytes" | "files" | "unknown"
	FilesTotal       uint64
	FilesCompleted   uint64
	BytesTotal       uint64
	BytesCompleted   uint64
	CoverageDegraded bool
}

type stStatusRead struct {
	TablePresent bool
	Latest       *stHeartbeat
	History      []stHeartbeat
}

type stCondition struct {
	Type       stConditionType
	State      stConditionState
	Reason     string
	ObservedAt int64
}

type stAlert struct {
	Code       stAlertCode
	ObservedAt int64
}

type stRate struct {
	BytesPerSecond float64
	SampleSeconds  uint64
}

type stEta struct {
	Scope       string
	LowSeconds  uint64
	HighSeconds uint64
}

type stHistoryPoint struct {
	TsUnixMs          int64
	QueueDepth        uint64
	FilesActive       uint32
	QueueCapacity     uint64
	SinkPendingRows   uint64
	SinkRetrying      bool
	DiscoveryComplete bool
	FilesTotal        uint64
	FilesCompleted    uint64
	BytesTotal        uint64
	BytesCompleted    uint64
}

func stHistoryPointFrom(h *stHeartbeat) stHistoryPoint {
	p := stHistoryPoint{
		TsUnixMs:    h.TsUnixMs,
		QueueDepth:  h.QueueDepth,
		FilesActive: h.FilesActive,
	}
	if h.Progress != nil {
		p.QueueCapacity = h.Progress.QueueCapacity
		p.SinkPendingRows = h.Progress.SinkPendingRows
		p.SinkRetrying = h.Progress.SinkRetrying
		p.DiscoveryComplete = h.Progress.DiscoveryComplete
		p.FilesTotal = h.Progress.FilesTotal
		p.FilesCompleted = h.Progress.FilesCompleted
		p.BytesTotal = h.Progress.BytesTotal
		p.BytesCompleted = h.Progress.BytesCompleted
	}
	return p
}

type stStatus struct {
	ObservedAt int64
	Current    *stHeartbeat
	Conditions []stCondition
	Alerts     []stAlert
	Rate       *stRate
	Eta        *stEta
	History    []stHistoryPoint
}

func stCondition2(t stConditionType, s stConditionState, reason string, now int64) stCondition {
	return stCondition{Type: t, State: s, Reason: reason, ObservedAt: now}
}

func stDerive(read stStatusRead, now int64) stStatus {
	latest := read.Latest
	health := stHealthCondition(latest, now)
	coverage := stCoverageCondition(latest, now)
	freshness := stFreshnessCondition(latest, health, now)
	readiness := stReadinessCondition(health, coverage, freshness, now)
	rate, eta := stRateAndEta(latest, read.History)
	alerts := stAlerts(latest, read.History, health, coverage, freshness, now)

	history := []stHistoryPoint{}
	if latest != nil {
		start := saturatingSub(latest.TsUnixMs, 300_000)
		for i := range read.History {
			ts := read.History[i].TsUnixMs
			if ts >= start && ts <= latest.TsUnixMs {
				history = append(history, stHistoryPointFrom(&read.History[i]))
			}
		}
	}
	sort.SliceStable(history, func(i, j int) bool { return history[i].TsUnixMs < history[j].TsUnixMs })
	if len(history) > 120 {
		history = history[len(history)-120:]
	}

	status := stStatus{
		ObservedAt: now,
		Conditions: []stCondition{health, coverage, freshness, readiness},
		Alerts:     alerts,
		Rate:       rate,
		Eta:        eta,
		History:    history,
	}
	if latest != nil {
		status.Current = latest
	}
	return status
}

func saturatingSub(a, b int64) int64 {
	if a < b {
		return 0
	}
	return a - b
}

func stHealthCondition(latest *stHeartbeat, now int64) stCondition {
	var state stConditionState
	var reason string
	switch {
	case latest == nil:
		state, reason = stStateUnknown, "heartbeat_missing"
	case latest.TsUnixMs > saturatingAdd(now, 5_000):
		state, reason = stStateUnknown, "heartbeat_clock_skew"
	case saturatingSub(now, latest.TsUnixMs) > 30_000:
		state, reason = stStateFalse, "heartbeat_stale"
	default:
		state, reason = stStateTrue, "heartbeat_recent"
	}
	return stCondition2(stCondHealth, state, reason, now)
}

func saturatingAdd(a, b int64) int64 {
	sum := a + b
	if (sum > a) != (b > 0) {
		if b > 0 {
			return 1<<63 - 1
		}
		return -1 << 63
	}
	return sum
}

func stCoverageCondition(latest *stHeartbeat, now int64) stCondition {
	if latest == nil || latest.Progress == nil {
		return stCondition2(stCondCoverage, stStateUnknown, "progress_unavailable", now)
	}
	progress := latest.Progress
	for _, source := range progress.Sources {
		if source.CoverageDegraded {
			return stCondition2(stCondCoverage, stStateFalse, "coverage_degraded", now)
		}
	}
	if !progress.DiscoveryComplete {
		return stCondition2(stCondCoverage, stStateFalse, "discovery_incomplete", now)
	}
	if progress.FilesCompleted < progress.FilesTotal || progress.BytesCompleted < progress.BytesTotal {
		return stCondition2(stCondCoverage, stStateFalse, "backfill_partial", now)
	}
	return stCondition2(stCondCoverage, stStateTrue, "backfill_complete", now)
}

func stFreshnessCondition(latest *stHeartbeat, health stCondition, now int64) stCondition {
	if health.State != stStateTrue {
		return stCondition2(stCondFreshness, stStateUnknown, health.Reason, now)
	}
	if latest.Progress == nil {
		return stCondition2(stCondFreshness, stStateUnknown, "progress_unavailable", now)
	}
	progress := latest.Progress
	hasPressure := latest.QueueDepth > 0 || latest.FilesActive > 0 ||
		progress.SinkPendingRows > 0 || progress.SinkRetrying
	lastProgress := progress.LastDurableProgressUnixMs
	if progress.RunStartedUnixMs > lastProgress {
		lastProgress = progress.RunStartedUnixMs
	}
	if stWorkRemaining(progress) && hasPressure && saturatingSub(now, int64(lastProgress)) >= 60_000 {
		return stCondition2(stCondFreshness, stStateFalse, "progress_stalled", now)
	}
	reason := "idle"
	if hasPressure {
		reason = "progress_recent"
	}
	return stCondition2(stCondFreshness, stStateTrue, reason, now)
}

func stWorkRemaining(progress *stProgress) bool {
	return progress.FilesCompleted < progress.FilesTotal ||
		progress.BytesCompleted < progress.BytesTotal
}

func stReadinessCondition(health, coverage, freshness stCondition, now int64) stCondition {
	conditions := []stCondition{health, coverage, freshness}
	for _, c := range conditions {
		if c.State == stStateFalse {
			return stCondition2(stCondReadiness, stStateFalse, "retrieval_may_be_incomplete", now)
		}
	}
	for _, c := range conditions {
		if c.State == stStateUnknown {
			return stCondition2(stCondReadiness, stStateUnknown, "readiness_unknown", now)
		}
	}
	return stCondition2(stCondReadiness, stStateTrue, "ready", now)
}

// stRateAndEta ports ingest_rate_and_eta. The sources adapter carries no
// instance ids, so the same-instance gates below hold trivially and the
// sample-count gate (>=6) decides.
func stRateAndEta(latest *stHeartbeat, history []stHeartbeat) (*stRate, *stEta) {
	if latest == nil || latest.Progress == nil {
		return nil, nil
	}
	latestProgress := latest.Progress
	if !latestProgress.DiscoveryComplete || latestProgress.SinkRetrying ||
		latestProgress.BytesCompleted >= latestProgress.BytesTotal {
		return nil, nil
	}

	windowStart := saturatingSub(latest.TsUnixMs, 300_000)
	var samples []*stHeartbeat
	for i := range history {
		h := &history[i]
		if h.TsUnixMs < windowStart || h.TsUnixMs > latest.TsUnixMs {
			continue
		}
		if h.Progress == nil || h.Progress.InstanceID != latestProgress.InstanceID {
			continue
		}
		samples = append(samples, h)
	}
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].TsUnixMs < samples[j].TsUnixMs })
	if len(samples) < 6 {
		return nil, nil
	}
	last := samples[len(samples)-1]
	if last.TsUnixMs != latest.TsUnixMs {
		return nil, nil
	}
	for _, sample := range samples {
		if !sample.Progress.DiscoveryComplete || sample.Progress.SinkRetrying {
			return nil, nil
		}
		if !stSameSnapshotTarget(sample.Progress, latestProgress) {
			return nil, nil
		}
	}
	for i := 1; i < len(samples); i++ {
		if !stProgressMonotonic(samples[i-1].Progress, samples[i].Progress) {
			return nil, nil
		}
	}
	first := samples[0]
	if saturatingSub(last.TsUnixMs, first.TsUnixMs) < 30_000 {
		return nil, nil
	}
	shortStart := saturatingSub(last.TsUnixMs, 60_000)
	var shortFirst *stHeartbeat
	for _, sample := range samples {
		if sample.TsUnixMs >= shortStart {
			shortFirst = sample
			break
		}
	}
	longRate := stSampleByteRate(first, last)
	if longRate == nil {
		return nil, nil
	}
	shortRate := stSampleByteRate(shortFirst, last)
	if shortRate == nil {
		return nil, nil
	}
	ratio := *shortRate / *longRate
	if ratio < 0.5 || ratio > 2.0 {
		return nil, nil
	}
	sampleSeconds := uint64(saturatingSub(last.TsUnixMs, first.TsUnixMs)) / 1_000
	remaining := float64(latestProgress.BytesTotal - latestProgress.BytesCompleted)
	maxRate := *shortRate
	if *longRate > maxRate {
		maxRate = *longRate
	}
	minRate := *shortRate
	if *longRate < minRate {
		minRate = *longRate
	}
	return &stRate{BytesPerSecond: *longRate, SampleSeconds: sampleSeconds},
		&stEta{Scope: "file_backfill",
			LowSeconds:  uint64(math.Ceil(remaining / maxRate)),
			HighSeconds: uint64(math.Ceil(remaining / minRate))}
}

func stSameSnapshotTarget(left, right *stProgress) bool {
	if left.InstanceID != right.InstanceID ||
		left.SnapshotUnixMs != right.SnapshotUnixMs ||
		left.FilesTotal != right.FilesTotal ||
		left.BytesTotal != right.BytesTotal ||
		len(left.Sources) != len(right.Sources) {
		return false
	}
	for i := range left.Sources {
		if left.Sources[i].SourceName != right.Sources[i].SourceName ||
			left.Sources[i].Format != right.Sources[i].Format ||
			left.Sources[i].CoverageBasis != right.Sources[i].CoverageBasis ||
			left.Sources[i].FilesTotal != right.Sources[i].FilesTotal ||
			left.Sources[i].BytesTotal != right.Sources[i].BytesTotal {
			return false
		}
	}
	return true
}

func stProgressMonotonic(previous, current *stProgress) bool {
	if !stSameSnapshotTarget(previous, current) {
		return false
	}
	if previous.FilesCompleted > current.FilesCompleted ||
		previous.BytesCompleted > current.BytesCompleted {
		return false
	}
	for i := range previous.Sources {
		if previous.Sources[i].FilesCompleted > current.Sources[i].FilesCompleted ||
			previous.Sources[i].BytesCompleted > current.Sources[i].BytesCompleted {
			return false
		}
	}
	return true
}

func stSampleByteRate(first, last *stHeartbeat) *float64 {
	if first.Progress == nil || last.Progress == nil {
		return nil
	}
	elapsed := last.TsUnixMs - first.TsUnixMs
	completed := int64(last.Progress.BytesCompleted) - int64(first.Progress.BytesCompleted)
	if elapsed <= 0 || completed == 0 {
		return nil
	}
	rate := float64(completed) / (float64(elapsed) / 1_000.0)
	return &rate
}

func stAlerts(latest *stHeartbeat, history []stHeartbeat, health, coverage, freshness stCondition, now int64) []stAlert {
	alerts := []stAlert{}
	if health.State == stStateFalse {
		alerts = append(alerts, stAlert{Code: stAlertHeartbeatStale, ObservedAt: now})
	}
	if freshness.Reason == "progress_stalled" {
		alerts = append(alerts, stAlert{Code: stAlertProgressStalled, ObservedAt: now})
	}
	if coverage.Reason == "coverage_degraded" {
		alerts = append(alerts, stAlert{Code: stAlertCoverageDegrade, ObservedAt: now})
	}
	if health.State != stStateTrue {
		return alerts
	}
	if latest == nil || latest.Progress == nil {
		return alerts
	}
	instanceID := latest.Progress.InstanceID
	windowStart := saturatingSub(latest.TsUnixMs, 300_000)
	var sameInstance []*stHeartbeat
	for i := range history {
		h := &history[i]
		if h.TsUnixMs < windowStart || h.TsUnixMs > latest.TsUnixMs {
			continue
		}
		if h.Progress == nil || h.Progress.InstanceID != instanceID {
			continue
		}
		sameInstance = append(sameInstance, h)
	}
	sort.SliceStable(sameInstance, func(i, j int) bool { return sameInstance[i].TsUnixMs < sameInstance[j].TsUnixMs })
	if len(sameInstance) == 0 || sameInstance[len(sameInstance)-1].TsUnixMs != latest.TsUnixMs {
		return alerts
	}
	saturated := len(sameInstance) >= 3
	for _, h := range sameInstance[max(0, len(sameInstance)-3):] {
		saturated = saturated && h.Progress.QueueCapacity > 0 && h.QueueDepth >= h.Progress.QueueCapacity
	}
	if saturated {
		alerts = append(alerts, stAlert{Code: stAlertQueueSaturated, ObservedAt: now})
	}
	retrying := len(sameInstance) >= 2
	for _, h := range sameInstance[max(0, len(sameInstance)-2):] {
		retrying = retrying && h.Progress.SinkRetrying
	}
	if retrying {
		alerts = append(alerts, stAlert{Code: stAlertSinkRetrying, ObservedAt: now})
	}
	return alerts
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
