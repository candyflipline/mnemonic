package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/export"
)

func init() { commands["export"] = exportCmd }

const exportUsage = `usage: mnemonic export atif|otlp --session S [--out F] [target]
       mnemonic export dataset --annotation N[=V] [--per-task K] [--include-private] [--out F] [target]
target: [--config C] [--server URL] [--token-file F] | --local [--server URL]

export asks the server for sessions from the index it serves, with the
server and token push uses (the client config, or --server and
--token-file), or a running mnemonic local with --local. It sees what the
token's MCP read tools see: a session they cannot open is "session not
found", and a deleted session is never exported. --include-private exports
every private session too, and needs an admin token over every author. A
session that fails ATIF validation is skipped by dataset with its id and
error on stderr; dataset fails only when every matching session did.`

const localServer = "http://127.0.0.1:7781"

// exportCmd streams one ATIF trajectory or OTLP trace, or a JSONL dataset
// filtered by annotation, from GET /v1/export/{kind} to --out.
func exportCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(exportUsage)
	}
	kind, rest := args[0], args[1:]
	switch kind {
	case "atif", "otlp", "dataset":
	default:
		return fmt.Errorf("export: unknown kind %q; %s", kind, exportUsage)
	}
	fs := flag.NewFlagSet("export "+kind, flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "client config holding server and token")
	server := fs.String("server", "", "server URL (default: the config's server, or "+localServer+" with --local)")
	tokenFile := fs.String("token-file", "", "file holding the bearer token (default: the config's token)")
	local := fs.Bool("local", false, "ask a running mnemonic local")
	session := fs.String("session", "", "session id (atif, otlp)")
	annotation := fs.String("annotation", "", "name[=value] to filter sessions by (dataset)")
	perTask := fs.Int("per-task", 3, "at most this many sessions per task (dataset)")
	includePrivate := fs.Bool("include-private", false, "also export private sessions (admin token)")
	out := fs.String("out", "", "output file (default stdout)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	q := url.Values{}
	if kind == "dataset" {
		if *annotation == "" {
			return errors.New("export: --annotation is required")
		}
		q.Set("annotation", *annotation)
		q.Set("per_task", strconv.Itoa(*perTask))
	} else {
		if *session == "" {
			return errors.New("export: --session is required")
		}
		q.Set("session", *session)
	}
	if *includePrivate {
		q.Set("include_private", "true")
	}
	base, token, err := exportTarget(*configPath, *server, *tokenFile, *local)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/v1/export/"+kind+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		io.Copy(io.Discard, resp.Body)
		printSkipped(resp.Trailer)
		return fmt.Errorf("export: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := writeOut(*out, resp.Body); err != nil {
		return err
	}
	printSkipped(resp.Trailer)
	return nil
}

// exportTarget is the server URL and token to ask: --local's, or the flags
// over the client config's.
func exportTarget(configPath, server, tokenFile string, local bool) (string, string, error) {
	if local {
		if server == "" {
			server = localServer
		}
		return server, "local", nil
	}
	var token string
	if tokenFile != "" {
		path, err := config.ExpandHome(tokenFile)
		if err != nil {
			return "", "", err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", "", fmt.Errorf("export: token file: %w", err)
		}
		token = strings.TrimSpace(string(raw))
	}
	if server == "" || token == "" {
		cfg, err := config.LoadClient(configPath)
		if err != nil {
			return "", "", err
		}
		if server == "" {
			server = cfg.Server
		}
		if token == "" {
			token = cfg.Token
		}
	}
	return server, token, nil
}

// writeOut copies body to path, or to stdout ending in a newline.
func writeOut(path string, body io.Reader) error {
	if path != "" && path != "-" {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, body); err != nil {
			f.Close()
			os.Remove(path)
			return fmt.Errorf("export: %w", err)
		}
		return f.Close()
	}
	last := &lastByte{w: os.Stdout}
	if _, err := io.Copy(last, body); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	if last.n > 0 && last.b != '\n' {
		_, err := os.Stdout.WriteString("\n")
		return err
	}
	return nil
}

type lastByte struct {
	w io.Writer
	n int64
	b byte
}

func (l *lastByte) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if n > 0 {
		l.n += int64(n)
		l.b = p[n-1]
	}
	return n, err
}

func printSkipped(trailer http.Header) {
	for _, v := range trailer.Values(export.SkippedTrailer) {
		fmt.Fprintf(os.Stderr, "export: skipped session %s\n", v)
	}
}
