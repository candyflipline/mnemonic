package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/candyflipline/mnemonic/arena/obj"
)

// runStore is `a store`: serves a file store's presigned GETs until a signal,
// standing in for S3 in tests.
func runStore(args []string) error {
	fs := flag.NewFlagSet("store", flag.ExitOnError)
	uri := fs.String("store", "", "file store URI with a serve= address")
	fs.Parse(args)
	u, err := url.Parse(*uri)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if u.Scheme != "file" {
		return fmt.Errorf("a store serves file stores only")
	}
	s, err := obj.Open(*uri)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("store: serving %s", u.Query().Get("serve"))
	return obj.Serve(ctx, s)
}
