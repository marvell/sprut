// Package cli is the whole sprut program as a function, so that it can be
// driven in-process with explicit args, environment and standard streams.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	ucli "github.com/urfave/cli/v3"
)

// Run executes sprut with the given args (args[0] is the program name),
// environment (in os.Environ form) and standard streams, and returns the
// process exit code.
func Run(ctx context.Context, args, env []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := &ucli.Command{
		Name:      "sprut",
		Usage:     "an MCP gateway that serves all your MCP servers as one",
		Reader:    stdin,
		Writer:    stdout,
		ErrWriter: stderr,
		// Errors are turned into exit codes below; never let urfave/cli call os.Exit.
		ExitErrHandler: func(context.Context, *ucli.Command, error) {},
		Action: func(_ context.Context, cmd *ucli.Command) error {
			// Bare sprut must never start the server; usage goes to stderr.
			cmd.Writer = stderr
			_ = ucli.ShowRootCommandHelp(cmd)
			return ucli.Exit("", 2)
		},
		Commands: []*ucli.Command{
			serveCommand(env, stdin, stdout, stderr),
		},
	}

	err := root.Run(ctx, args)
	if err == nil {
		return 0
	}
	var exitErr ucli.ExitCoder
	if errors.As(err, &exitErr) {
		if msg := exitErr.Error(); msg != "" {
			fmt.Fprintln(stderr, msg)
		}
		return exitErr.ExitCode()
	}
	fmt.Fprintln(stderr, "sprut:", err)
	return 1
}
