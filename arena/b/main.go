package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: b ingest|batch -root ROOT -store STORE")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	root := fs.String("root", "", "raw data root")
	dir := fs.String("store", "", "store directory")
	if e := fs.Parse(os.Args[2:]); e != nil {
		return e
	}
	if *root == "" || *dir == "" {
		return fmt.Errorf("root and store required")
	}
	if os.Args[1] == "ingest" {
		return ingest(*root, *dir)
	}
	if os.Args[1] != "batch" {
		return fmt.Errorf("unknown command")
	}
	db, e := loadStore(*dir)
	if e != nil {
		return e
	}
	debug.FreeOSMemory()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for scanner.Scan() {
		var call struct {
			ID   any             `json:"id"`
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		}
		if e = json.Unmarshal(scanner.Bytes(), &call); e != nil {
			return e
		}
		start := time.Now()
		var data any
		var text, errText string
		if t, ok := Lookup(call.Tool); ok {
			var err error
			data, text, err = t.Call(context.Background(), db, call.Args)
			if err != nil {
				errText = err.Error()
			}
		} else {
			errText = "unknown tool: " + call.Tool
		}
		out := struct {
			ID    any    `json:"id"`
			Data  any    `json:"data"`
			Text  string `json:"text"`
			Error string `json:"error"`
			US    int64  `json:"us"`
		}{call.ID, data, text, errText, time.Since(start).Microseconds()}
		if e = json.NewEncoder(w).Encode(out); e != nil {
			return e
		}
		if e = w.Flush(); e != nil {
			return e
		}
	}
	return scanner.Err()
}
