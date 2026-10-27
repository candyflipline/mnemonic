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
	"os"
	"strings"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/qnet"
)

func init() {
	commands["key"] = keyCmd
	commands["mcp"] = mcpCmd
}

// keyCmd prints the public key of a key file (made if missing) and the
// [[keys]] entry that lets it in.
func keyCmd(args []string) error {
	fs := flag.NewFlagSet("key", flag.ContinueOnError)
	file := fs.String("file", config.DefaultKeyFile, "ed25519 key file")
	author := fs.String("author", defaultAuthor(), "author for the [[keys]] entry")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return printKey(*file, *author)
}

func printKey(file, author string) error {
	path, err := config.ExpandHome(file)
	if err != nil {
		return err
	}
	priv, err := qnet.LoadOrCreateKey(path)
	if err != nil {
		return err
	}
	pub := qnet.PublicKey(priv)
	fmt.Println(pub)
	fmt.Printf("# set author to the person this machine belongs to\n[[keys]]\nkey = %q\nauthor = %q\n", pub, author)
	return nil
}

func defaultAuthor() string {
	if u := strings.ToLower(os.Getenv("USER")); u != "" {
		return u
	}
	return "AUTHOR"
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
	hc, err := quicClient(cfg)
	if err != nil {
		return err
	}
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

// quicClient is an http.Client that reaches the server's HTTP endpoint over
// QUIC, pinned to cfg.ServerKey and signed by cfg.KeyFile. Its requests go to
// http://mnemonic/.
func quicClient(cfg config.Client) (*http.Client, error) {
	priv, err := qnet.LoadOrCreateKey(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return qnet.Dial(ctx, cfg.Addr(), priv, cfg.ServerKey, qnet.ALPNHTTP)
		},
	}}, nil
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
