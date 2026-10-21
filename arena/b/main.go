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
	defer func() { db.Close() }()
	debug.FreeOSMemory()
	type inputLine struct {
		raw []byte
		err error
	}
	lines := make(chan inputLine, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			lines <- inputLine{raw: append([]byte(nil), scanner.Bytes()...)}
		}
		if err := scanner.Err(); err != nil {
			lines <- inputLine{err: err}
		}
	}()
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	idle := time.NewTimer(300 * time.Millisecond)
	defer idle.Stop()
	for {
		var input inputLine
		select {
		case next, ok := <-lines:
			if !ok {
				return nil
			}
			input = next
		case <-idle.C:
			if len(lines) == 0 {
				db.releaseIdlePages()
				debug.FreeOSMemory()
			}
			continue
		}
		if input.err != nil {
			return input.err
		}
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		var call struct {
			ID   any             `json:"id"`
			Tool string          `json:"tool"`
			Args json.RawMessage `json:"args"`
		}
		if e = json.Unmarshal(input.raw, &call); e != nil {
			return e
		}
		start := time.Now()
		if stampManifest(*dir) != db.stamp {
			fresh, err := loadStore(*dir)
			if err != nil {
				return err
			}
			old := db
			db = fresh
			old.Close()
		}
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
		idle.Reset(300 * time.Millisecond)
	}
}
