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
	code := cli.Run(ctx, os.Args, os.Environ(), os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
