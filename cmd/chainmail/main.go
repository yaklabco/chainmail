// Command chainmail lets AI agents read, write and organize email through a policy daemon.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	// The binary carries its own time zone database, so [rules] timezone
	// works on hosts without zoneinfo.
	_ "time/tzdata"

	"github.com/yaklabco/chainmail/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, os.Args[1:], cli.DefaultEnv())
	stop()
	os.Exit(code)
}
