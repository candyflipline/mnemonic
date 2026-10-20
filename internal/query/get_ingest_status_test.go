package query

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const stMarker = "SECRET_MARKER_7f3a"

func stRawHeartbeat(tsUnixMs int64) stHeartbeat {
	backend := "secret-backend-" + stMarker
	watcherErrors := uint64(4)
	return stHeartbeat{
		TsUnixMs:          tsUnixMs,
		Host:              "/Users/alice/private/" + stMarker + ".jsonl",
		ServiceVersion:    "9.9.9-internal-" + stMarker,
		QueueDepth:        3,
		FilesActive:       2,
		FilesWatched:      40,
		LastError:         "password=hunter2 " + stMarker,
		WatcherBackend:    &backend,
		WatcherErrorCount: &watcherErrors,
		BackendSinks:      `{"sinks":[{"credential":"SECRET_SINK_` + stMarker + `","source_path":"/srv/hidden/` + stMarker + `"}]}`,
		Progress: &stProgress{
			SchemaVersion:     1,
			InstanceID:        "instance-" + stMarker,
			RunStartedUnixMs:  1000,
			SnapshotUnixMs:    2000,
			DiscoveryComplete: true,
			QueueCapacity:     64,
			SinkPendingRows:   8,
			SinkPendingBytes:  96,
			FilesTotal:        10,
			FilesCompleted:    8,
			BytesTotal:        10000,
			BytesCompleted:    4000,
			Sources: []stSourceProgress{{
				SourceName:     "vendor-" + stMarker,
				Format:         "jsonl",
				CoverageBasis:  "bytes",
				FilesTotal:     10,
				FilesCompleted: 8,
				BytesTotal:     10000,
				BytesCompleted: 4000,
			}},
		},
	}
}

func TestStExplicitStatusProjectionRedactsLatestAndRawHistoryRecursively(t *testing.T) {
	latest := stRawHeartbeat(120000)
	history := stRawHeartbeat(60000)
	read := stStatusRead{TablePresent: true, Latest: &latest, History: []stHeartbeat{history}}
	status := stDerive(read, 120001)

	raw, err := json.Marshal(stStatusJSON(status))
	if err != nil {
		t.Fatal(err)
	}
	projected := string(raw)
	for _, marker := range []string{
		stMarker, "SECRET_SINK_", "password=hunter2", "/Users/alice", "9.9.9-internal", "secret-backend",
	} {
		if strings.Contains(projected, marker) {
			t.Fatalf("projection leaks %q:\n%s", marker, projected)
		}
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"heartbeat": true, "host": true, "service_version": true, "last_error": true,
		"watcher_backend": true, "watcher_error_count": true, "watcher_reset_count": true,
		"watcher_last_reset_unix_ms": true, "backend_sinks": true, "sources": true,
		"source_name": true, "instance_id": true, "run_started_unix_ms": true, "snapshot_unix_ms": true,
	}
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch v := node.(type) {
		case map[string]any:
			for key, child := range v {
				if forbidden[key] {
					t.Fatalf("forbidden key %s at %s", key, path)
				}
				walk(path+"."+key, child)
			}
		case []any:
			for i, child := range v {
				walk(sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("", doc)

	if doc["observed_at_unix_ms"] != float64(120001) {
		t.Fatalf("observed_at = %v", doc["observed_at_unix_ms"])
	}
	current := doc["current"].(map[string]any)
	if current["heartbeat_observed_at_unix_ms"] != float64(120000) || current["queue_depth"] != float64(3) {
		t.Fatalf("current = %#v", current)
	}
	progress := current["progress"].(map[string]any)
	if progress["files_total"] != float64(10) || progress["files_completed"] != float64(8) {
		t.Fatalf("progress = %#v", progress)
	}
	alerts := doc["alerts"].([]any)
	if len(alerts) != 1 || alerts[0].(map[string]any)["code"] != "progress_stalled" {
		t.Fatalf("alerts = %#v", alerts)
	}
	want := map[string]any{
		"ts_unix_ms": float64(60000), "queue_depth": float64(3), "files_active": float64(2),
		"queue_capacity": float64(64), "sink_pending_rows": float64(8), "sink_retrying": false,
		"discovery_complete": true, "files_total": float64(10), "files_completed": float64(8),
		"bytes_total": float64(10000), "bytes_completed": float64(4000),
	}
	if got := doc["history"].([]any)[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("history[0] = %#v", got)
	}
}

func TestStInvalidArgumentsAreNotReflected(t *testing.T) {
	raw := json.RawMessage(`{"credential":"` + stMarker + `","path":"/etc/shadow"}`)
	_, text, err := (getIngestStatus{}).Call(context.Background(), nil, raw)
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != "get_ingest_status expects an empty JSON object" {
		t.Fatalf("error = %v", err)
	}
	if text != "" || strings.Contains(err.Error(), stMarker) {
		t.Fatalf("arguments reflected: %q / %q", text, err)
	}
}

func TestStEmptyArgs(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{}`, true},
		{` []`, false},
		{`null`, false},
		{`{"a":1}`, false},
		{`not json`, false},
	} {
		if got := stEmptyArgs(json.RawMessage(tc.raw)); got != tc.want {
			t.Fatalf("stEmptyArgs(%s) = %v", tc.raw, got)
		}
	}
}
