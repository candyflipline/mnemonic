package query

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const stMarker = "SECRET_MARKER_7f3a"

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
