package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/candyflipline/mnemonic/internal/auth"
	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/qnet"
)

// keyCmd prints the public key of a key file (made if missing) and the
// [[tokens]] entry that lets it in.
func keyCmd(args []string) error {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	file := fs.String("file", "~/.mnemonic/key", "ed25519 seed file")
	author := fs.String("author", "AUTHOR", "author for the [[tokens]] entry")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := config.ExpandHome(*file)
	if err != nil {
		return err
	}
	priv, err := qnet.LoadOrCreateKey(path)
	if err != nil {
		return err
	}
	pub := qnet.PublicKey(priv)
	fmt.Println(pub)
	fmt.Printf("[[tokens]]\nauthor = %q\nsha256 = %q\n", *author, auth.Hash(pub))
	return nil
}

// mcpCmd is a stdio MCP server that forwards each JSON-RPC line to the
// server's /mcp over QUIC and writes each reply as one line.
func mcpCmd(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	u, err := url.Parse(cfg.Server)
	if err != nil || u.Scheme != "quic" {
		return fmt.Errorf("mcp needs a quic:// server, not %q", cfg.Server)
	}
	priv, err := qnet.LoadOrCreateKey(cfg.KeyFile)
	if err != nil {
		return err
	}
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return qnet.Dial(ctx, u.Host, priv, cfg.ServerKey, qnet.ALPNMCP)
		},
	}}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 64<<20)
	out := bufio.NewWriter(os.Stdout)
	var h mcpHeaders
	for in.Scan() {
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}
		reply, err := forward(hc, line, &h)
		if err != nil {
			reply = rpcError(line, err)
		}
		if len(reply) == 0 {
			continue
		}
		out.Write(reply)
		out.WriteByte('\n')
		out.Flush()
	}
	return in.Err()
}

// mcpHeaders are what Streamable HTTP wants back after initialize.
type mcpHeaders struct{ session, version string }

func forward(hc *http.Client, line []byte, h *mcpHeaders) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, "http://mnemonic/mcp", bytes.NewReader(line))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if h.session != "" {
		req.Header.Set("Mcp-Session-Id", h.session)
	}
	if h.version != "" {
		req.Header.Set("MCP-Protocol-Version", h.version)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.session = sid
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}
	var msg struct {
		JSONRPC string `json:"jsonrpc"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.JSONRPC == "" {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	if msg.Result.ProtocolVersion != "" {
		h.version = msg.Result.ProtocolVersion
	}
	var compact bytes.Buffer
	json.Compact(&compact, body)
	return compact.Bytes(), nil
}

func rpcError(line []byte, err error) []byte {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 {
		fmt.Fprintln(os.Stderr, "mnemonic mcp:", err)
		return nil
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"error":   map[string]any{"code": -32000, "message": err.Error()},
	})
	return b
}
