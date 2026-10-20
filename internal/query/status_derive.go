package query

import (
	"math"
	"sort"
)

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
	stAlertHeartbeatStale stAlertCode = "heartbeat_stale"
)

type stHeartbeat struct {
	TsUnixMs                  int64
	Host                      string
	FilesWatched              uint32
	LastDurableProgressUnixMs uint64
	FilesTotal                uint64
	FilesCompleted            uint64
	BytesTotal                uint64
	BytesCompleted            uint64
}

type stStatusRead struct {
	Latest  *stHeartbeat
	History []stHeartbeat
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

type stStatus struct {
	ObservedAt int64
	Current    *stHeartbeat
	Conditions []stCondition
	Alerts     []stAlert
	Rate       *stRate
	Eta        *stEta
	History    []stHeartbeat
}

func stCondition2(t stConditionType, s stConditionState, reason string, now int64) stCondition {
	return stCondition{Type: t, State: s, Reason: reason, ObservedAt: now}
}

func stDerive(read stStatusRead, now int64) stStatus {
	latest := read.Latest
	health := stHealthCondition(latest, now)
	coverage := stCoverageCondition(latest, now)
	freshness := stFreshnessCondition(health, now)
	readiness := stReadinessCondition(health, coverage, freshness, now)
	rate, eta := stRateAndEta(latest, read.History)
	alerts := []stAlert{}
	if health.State == stStateFalse {
		alerts = append(alerts, stAlert{Code: stAlertHeartbeatStale, ObservedAt: now})
	}

	history := []stHeartbeat{}
	if latest != nil {
		start := saturatingSub(latest.TsUnixMs, 300_000)
		for _, h := range read.History {
			if h.TsUnixMs >= start && h.TsUnixMs <= latest.TsUnixMs {
				history = append(history, h)
			}
		}
	}
	sort.SliceStable(history, func(i, j int) bool { return history[i].TsUnixMs < history[j].TsUnixMs })
	if len(history) > 120 {
		history = history[len(history)-120:]
	}

	return stStatus{
		ObservedAt: now,
		Current:    latest,
		Conditions: []stCondition{health, coverage, freshness, readiness},
		Alerts:     alerts,
		Rate:       rate,
		Eta:        eta,
		History:    history,
	}
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
	if latest == nil {
		return stCondition2(stCondCoverage, stStateUnknown, "progress_unavailable", now)
	}
	if latest.FilesCompleted < latest.FilesTotal || latest.BytesCompleted < latest.BytesTotal {
		return stCondition2(stCondCoverage, stStateFalse, "backfill_partial", now)
	}
	return stCondition2(stCondCoverage, stStateTrue, "backfill_complete", now)
}

func stFreshnessCondition(health stCondition, now int64) stCondition {
	if health.State != stStateTrue {
		return stCondition2(stCondFreshness, stStateUnknown, health.Reason, now)
	}
	return stCondition2(stCondFreshness, stStateTrue, "idle", now)
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

func stRateAndEta(latest *stHeartbeat, history []stHeartbeat) (*stRate, *stEta) {
	if latest == nil || latest.BytesCompleted >= latest.BytesTotal {
		return nil, nil
	}

	windowStart := saturatingSub(latest.TsUnixMs, 300_000)
	var samples []*stHeartbeat
	for i := range history {
		h := &history[i]
		if h.TsUnixMs < windowStart || h.TsUnixMs > latest.TsUnixMs {
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
	for i, sample := range samples {
		if sample.FilesTotal != latest.FilesTotal || sample.BytesTotal != latest.BytesTotal {
			return nil, nil
		}
		if i > 0 && (samples[i-1].FilesCompleted > sample.FilesCompleted ||
			samples[i-1].BytesCompleted > sample.BytesCompleted) {
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
	remaining := float64(latest.BytesTotal - latest.BytesCompleted)
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

func stSampleByteRate(first, last *stHeartbeat) *float64 {
	elapsed := last.TsUnixMs - first.TsUnixMs
	completed := int64(last.BytesCompleted) - int64(first.BytesCompleted)
	if elapsed <= 0 || completed == 0 {
		return nil
	}
	rate := float64(completed) / (float64(elapsed) / 1_000.0)
	return &rate
}
