package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/TrueFix/keepsync/internal/keepsync"
)

var version = "dev"

func main() {
	flags := flag.NewFlagSet("keepsync", flag.ContinueOnError)
	config := flags.String("config", "", "optional JSON configuration file (otherwise use environment variables)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: keepsync [--config FILE] COMMAND [OPTIONS]\nCommands: init, register-source, backup, snapshot, serve, snapshots, restore, check, prune, status, version")
		flags.PrintDefaults()
	}
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	args := flags.Args()
	if len(args) == 0 {
		flags.Usage()
		os.Exit(2)
	}
	if args[0] == "version" && len(args) == 1 {
		fmt.Printf("KeepSync %s\n", version)
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cfg, err := keepsync.LoadConfig(*config)
	if err == nil {
		err = keepsync.New(cfg, os.Stdout, os.Stderr).Run(ctx, args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "keepsync:", err)
		os.Exit(1)
	}
}
