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
		// urfave/cli's own --version flag also takes -v, which serve uses
		// for --verbose.
		HideVersion: true,
		Flags: []ucli.Flag{
			&ucli.BoolFlag{Name: "version", Usage: "print the version", Local: true},
		},
		// Before, not Action, so that --version wins over a subcommand too.
		Before: func(ctx context.Context, cmd *ucli.Command) (context.Context, error) {
			if cmd.Bool("version") {
				_, _ = fmt.Fprintf(stdout, "sprut version %s\n", version())
				return ctx, ucli.Exit("", 0)
			}
			return ctx, nil
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			// Bare sprut must never start the server.
			return usageError(ctx, cmd, nil)
		},
		Commands: []*ucli.Command{
			serveCommand(env, stdin, stdout, stderr),
			authCommand(env, stdout, stderr),
		},
	}
	// An unknown help topic (sprut help bogus) can only be reported from a
	// callback that returns nothing, so its usage error is kept here.
	var helpErr error
	strictUsage(root, &helpErr)

	err := root.Run(ctx, args)
	if err == nil {
		err = helpErr
	}
	if err == nil {
		return 0
	}
	var exitErr ucli.ExitCoder
	if errors.As(err, &exitErr) {
		if msg := exitErr.Error(); msg != "" {
			_, _ = fmt.Fprintln(stderr, msg)
		}
		return exitErr.ExitCode()
	}
	_, _ = fmt.Fprintln(stderr, "sprut:", err)
	return 1
}

// strictUsage makes every usage error in the tree under c a usageError: a
// bad flag or flag value, a missing argument, and any positional argument
// beyond those a command declares. An unknown help topic's usage error goes
// to *helpErr instead.
func strictUsage(c *ucli.Command, helpErr *error) {
	c.OnUsageError = func(ctx context.Context, cmd *ucli.Command, err error, _ bool) error {
		return usageError(ctx, cmd, err)
	}
	// Decided now: by the time an Action runs, urfave/cli has given every
	// command a help subcommand.
	unexpected := "unexpected argument"
	if len(c.Commands) > 0 {
		unexpected = "unknown command"
	}
	c.CommandNotFound = func(ctx context.Context, cmd *ucli.Command, name string) {
		*helpErr = usageError(ctx, cmd, fmt.Errorf("%s %q", unexpected, name))
	}
	if action := c.Action; action != nil {
		c.Action = func(ctx context.Context, cmd *ucli.Command) error {
			if cmd.Args().Present() {
				return usageError(ctx, cmd, fmt.Errorf("%s %q", unexpected, cmd.Args().First()))
			}
			return action(ctx, cmd)
		}
	}
	for _, sub := range c.Commands {
		strictUsage(sub, helpErr)
	}
}

// usageError prints err, if any, and the help of cmd to stderr, and returns
// the error that makes sprut exit 2. Help on stdout would corrupt the MCP
// channel of serve, and urfave/cli prints all help to the root's Writer.
func usageError(ctx context.Context, cmd *ucli.Command, err error) error {
	root := cmd.Root()
	if err != nil {
		_, _ = fmt.Fprintf(root.ErrWriter, "sprut: %s\n\n", err)
	}
	root.Writer = root.ErrWriter
	if cmd == root {
		_ = ucli.ShowRootCommandHelp(cmd)
	} else {
		parent := cmd.Lineage()[1]
		_ = ucli.ShowCommandHelp(ctx, parent, cmd.Name)
	}
	return ucli.Exit("", 2)
}
