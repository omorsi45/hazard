package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/omorsi45/hazard/internal/cli"
)

func main() {
	// Ctrl-C and SIGTERM cancel the context instead of ending the process, so
	// Execute returns normally and deferred cleanup (mutation slots, temporary
	// files) runs. syscall.SIGTERM exists on Windows too, where only the
	// interrupt is ever delivered.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop() // os.Exit skips deferred calls
	os.Exit(code)
}
