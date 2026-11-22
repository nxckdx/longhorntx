// Command lhtx is a low-traffic TUI and CLI for Longhorn.
// Installed as kubectl-lhtx it also works as a kubectl plugin; linked as
// kubectl_complete-lhtx it provides completion for `kubectl lhtx`.
package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nxckdx/longhorntx/internal/cli"
)

func main() {
	// kubectl (≥ 1.26) completes plugin arguments by running
	// kubectl_complete-<plugin> with the words typed so far and expects
	// cobra's __complete output.
	if strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl_complete-") {
		os.Args = append([]string{"kubectl-lhtx", "__complete"}, os.Args[1:]...)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx)
	stop()
	os.Exit(code)
}
