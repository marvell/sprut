// Command sprut is an MCP gateway: one MCP server that gives any Agent all
// of the user's MCP servers at once.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/marvell/sprut/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Restore the default signal behavior once the first signal cancels ctx,
	// so a second Ctrl+C or SIGTERM kills a shutdown that hangs.
	go func() { <-ctx.Done(); stop() }()
	code := cli.Run(ctx, os.Args, os.Environ(), os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
