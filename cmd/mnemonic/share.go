package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/candyflipline/mnemonic/internal/config"
	"github.com/candyflipline/mnemonic/internal/index"
)

func init() {
	commands["share"] = share
}

func share(args []string) error {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
	title := fs.String("title", "", "the link's title")
	note := fs.String("note", "", "a note shown under the title")
	days := fs.Int("days", 0, "days until the link expires (default 30)")
	var ids []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		ids = append(ids, sessionRef(fs.Arg(0)))
		rest = fs.Args()[1:]
	}
	if len(ids) == 0 {
		return fmt.Errorf("usage: mnemonic share <session id or Claude Code session uuid>... [--title T] [--note N] [--days D]")
	}
	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	hc, err := quicClient(cfg)
	if err != nil {
		return err
	}
	callArgs := map[string]any{"session_ids": ids}
	if *title != "" {
		callArgs["title"] = *title
	}
	if *note != "" {
		callArgs["note"] = *note
	}
	if *days != 0 {
		callArgs["days"] = *days
	}
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "share_link", "arguments": callArgs}})
	reply, err := forward(hc, line, &mcpHeaders{version: "2025-06-18"})
	if err != nil {
		return err
	}
	var out struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent struct {
				Data struct {
					URL string `json:"url"`
				} `json:"data"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(reply, &out); err != nil {
		return fmt.Errorf("share: %s", reply)
	}
	if out.Error != nil {
		return fmt.Errorf("share: %s", out.Error.Message)
	}
	var text []string
	for _, c := range out.Result.Content {
		text = append(text, c.Text)
	}
	if out.Result.IsError {
		return fmt.Errorf("share: %s", strings.Join(text, "\n"))
	}
	fmt.Println(strings.Join(text, "\n"))
	if url := out.Result.StructuredContent.Data.URL; url != "" && runtime.GOOS == "darwin" {
		pb := exec.Command("pbcopy")
		pb.Stdin = strings.NewReader(url)
		if pb.Run() == nil {
			fmt.Println("copied the URL to the clipboard")
		}
	}
	return nil
}

// sessionRef is arg as a session id: itself when it already is one, else
// the session whose raw id it is, as a Claude Code session uuid is.
func sessionRef(arg string) string {
	if strings.HasPrefix(arg, "session:") {
		return arg
	}
	return index.FormatSessionID(arg)
}
