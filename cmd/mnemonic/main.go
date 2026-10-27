package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/candyflipline/mnemonic/internal/config"
	pushrun "github.com/candyflipline/mnemonic/internal/push"
	"github.com/candyflipline/mnemonic/internal/redact"
)

var commands = map[string]func(args []string) error{
	"push": push,
	"init": initCmd,
}

func main() {
	if len(os.Args) < 2 || commands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: mnemonic init|key|mcp|push|serve|wallet [flags]")
		os.Exit(2)
	}
	if err := commands[os.Args[1]](os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "mnemonic:", err)
		os.Exit(1)
	}
}

func push(args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	configPath := fs.String("config", "~/.mnemonic/config.toml", "path to the client config TOML")
	watch := fs.Bool("watch", false, "stay resident and push each write within 5 s")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	red, err := redact.New(cfg.Redaction.ExtraPatterns)
	if err != nil {
		return err
	}
	if *watch {
		runtime.GOMAXPROCS(1)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return pushrun.Watch(ctx, cfg, red, os.Stdout)
	}
	stats, err := pushrun.Run(context.Background(), cfg, red)
	if err != nil {
		return err
	}
	fmt.Println(stats)
	return nil
}
