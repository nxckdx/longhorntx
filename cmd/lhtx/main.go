// Command lhtx is a low-traffic TUI and CLI for Longhorn.
// Installed as kubectl-lhtx it also works as a kubectl plugin.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nxckdx/longhorntx/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx)
	stop()
	os.Exit(code)
}
