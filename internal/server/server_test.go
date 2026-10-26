package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/layout"
)

const bootPrefix = "Index boot failing, retrying: attempt "

// bootLine is get_ingest_status's boot-failure text line, "" when it has none.
func bootLine(t *testing.T, url string) string {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_ingest_status","arguments":{}}}`
	req, err := http.NewRequest(http.MethodPost, url+"/mcp", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Result.IsError {
		t.Fatalf("get_ingest_status answered an error")
	}
	for _, c := range out.Result.Content {
		for _, line := range strings.Split(c.Text, "\n") {
			if strings.HasPrefix(line, bootPrefix) {
				return line
			}
		}
	}
	return ""
}

func TestBootFailureKeepsServeUpAndRetries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	bootFirstDelay, bootMaxDelay = 20*time.Millisecond, 40*time.Millisecond
	dir := t.TempDir()
	harness := filepath.Join(dir, "events", "author=a", "machine="+layout.ArchiveMachine, "harness=h")
	if err := os.MkdirAll(harness, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(harness, "ch-1.parquet"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(harness, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(harness, 0o700) })
	srv, err := New(dir, auth.NewTokens(map[string]auth.Token{auth.Hash("tok"): {Author: "a"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	web := httptest.NewServer(srv.Handler())
	defer web.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ran := make(chan struct{})
	go func() { srv.Run(ctx); close(ran) }()

	var failing string
	for deadline := time.Now().Add(5 * time.Second); failing == "" || strings.HasPrefix(failing, bootPrefix+"1:"); {
		select {
		case <-ran:
			t.Fatal("Run ended; serve must stay up and retry the boot")
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("get_ingest_status never showed a boot failure past attempt 1: %q", failing)
		}
		failing = bootLine(t, web.URL)
	}
	if !strings.Contains(failing, "permission denied") {
		t.Fatalf("boot line does not carry the boot error: %q", failing)
	}
	select {
	case <-srv.Booted():
		t.Fatal("Booted closed while the boot was failing: push would be served")
	default:
	}

	if err := os.Chmod(harness, 0o700); err != nil {
		t.Fatal(err)
	}
	select {
	case <-srv.Booted():
	case <-ran:
		t.Fatal("Run ended before the boot succeeded")
	case <-time.After(5 * time.Second):
		t.Fatal("boot did not succeed after the failure was cleared")
	}
	if l := bootLine(t, web.URL); l != "" {
		t.Fatalf("get_ingest_status still reports a boot failure after boot: %q", l)
	}
}
