package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/qnet"
	"github.com/candyflipline/mnemonic/internal/wire"
)

func init() {
	commands["join"] = join
}

func join(args []string) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	machine := fs.String("machine", "", "machine name (default: short hostname)")
	force := fs.Bool("force", false, "overwrite an existing config.toml")
	label := fs.String("label", launchdLabel, "launchd label (macOS)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: mnemonic join <invite url>")
	}
	invite, err := inviteURL(fs.Arg(0))
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("join: home: %w", err)
	}
	configPath := filepath.Join(home, ".mnemonic", "config.toml")
	if runtime.GOOS == "darwin" {
		if err := checkLaunchdTakeover(home, *label, *force); err != nil {
			return err
		}
	}
	if _, err := os.Stat(configPath); err == nil && !*force {
		return fmt.Errorf("%s already exists, so this machine has joined a server; pass --force to replace its config", configPath)
	}
	name, err := machineName(*machine)
	if err != nil {
		return err
	}
	keyFile, err := config.ExpandHome(config.DefaultKeyFile)
	if err != nil {
		return err
	}
	priv, err := qnet.LoadOrCreateKey(keyFile)
	if err != nil {
		return err
	}
	ans, err := postJoin(invite, qnet.PublicKey(priv), name)
	if err != nil {
		return err
	}
	fmt.Printf("joined %s as %s\n", ans.Server, ans.Author)
	if err := saveConfig(configPath, clientConfig("quic://"+ans.Server, ans.ServerKey, name), *force); err != nil {
		return err
	}
	if err := installScheduler(home, *label); err != nil {
		return err
	}
	fmt.Print(`
Point your agents at the server. Claude Code:
  claude mcp add-json --scope user mnemonic '{"type":"stdio","command":"mnemonic","args":["mcp"]}'
Codex, in ~/.codex/config.toml:
  [mcp_servers.mnemonic]
  command = "mnemonic"
  args = ["mcp"]
`)
	return nil
}

// inviteURL is raw as the URL a join posts to: an https URL (http only for a
// loopback host) whose path is /i/<token>, with any .md suffix dropped. The
// answer carries the server key this machine will trust, so plaintext would
// let anyone on the path substitute their own.
func inviteURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("%q is not an invite URL (https://host/i/<token>)", raw)
	}
	if u.Scheme == "http" && !config.IsLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("%q is plaintext http, and the answer carries the key this machine will trust; use the https:// invite URL", raw)
	}
	token, ok := strings.CutPrefix(strings.TrimSuffix(u.Path, ".md"), "/i/")
	if !ok || token == "" || strings.Contains(token, "/") {
		return "", fmt.Errorf("%q is not an invite URL (https://host/i/<token>)", raw)
	}
	u.Path, u.RawQuery, u.Fragment = "/i/"+token, "", ""
	return u.String(), nil
}

func postJoin(invite, key, machine string) (wire.JoinAnswer, error) {
	body, _ := json.Marshal(wire.JoinRequest{Key: key, Machine: machine})
	// A redirect could point the POST at a host inviteURL never checked, and
	// the answer is the server key this machine will trust.
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Post(invite, "application/json", bytes.NewReader(body))
	if err != nil {
		return wire.JoinAnswer{}, fmt.Errorf("join: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return wire.JoinAnswer{}, fmt.Errorf("join: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return wire.JoinAnswer{}, fmt.Errorf("join: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var ans wire.JoinAnswer
	if err := json.Unmarshal(b, &ans); err != nil || ans.Server == "" || ans.ServerKey == "" || ans.Author == "" {
		return wire.JoinAnswer{}, fmt.Errorf("join: the server answered %q, not a join", b)
	}
	return ans, nil
}
