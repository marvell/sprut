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
		Action: func(_ context.Context, cmd *ucli.Command) error {
			if cmd.Bool("version") {
				_, _ = fmt.Fprintf(stdout, "sprut version %s\n", version())
				return nil
			}
			// Bare sprut must never start the server.
			return usageError(cmd, nil)
		},
		Commands: []*ucli.Command{
			serveCommand(env, stdin, stdout, stderr),
		},
	}
	strictUsage(root)

	err := root.Run(ctx, args)
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

// strictUsage makes every usage error in the tree under cmd a usageError:
// a bad flag or flag value, and any positional argument, which no command
// takes.
func strictUsage(cmd *ucli.Command) {
	cmd.OnUsageError = func(_ context.Context, cmd *ucli.Command, err error, _ bool) error {
		return usageError(cmd, err)
	}
	// Decided now: by the time an Action runs, urfave/cli has given every
	// command a help subcommand.
	unexpected := "unexpected argument"
	if len(cmd.Commands) > 0 {
		unexpected = "unknown command"
	}
	if action := cmd.Action; action != nil {
		cmd.Action = func(ctx context.Context, cmd *ucli.Command) error {
			if cmd.Args().Present() {
				return usageError(cmd, fmt.Errorf("%s %q", unexpected, cmd.Args().First()))
			}
			return action(ctx, cmd)
		}
	}
	for _, sub := range cmd.Commands {
		strictUsage(sub)
	}
}

// usageError prints err, if any, and the help of cmd to stderr, and returns
// the error that makes sprut exit 2. Help on stdout would corrupt the MCP
// channel of serve, and urfave/cli prints all help to the root's Writer.
func usageError(cmd *ucli.Command, err error) error {
	root := cmd.Root()
	if err != nil {
		_, _ = fmt.Fprintf(root.ErrWriter, "sprut: %s\n\n", err)
	}
	root.Writer = root.ErrWriter
	if cmd == root {
		_ = ucli.ShowRootCommandHelp(cmd)
	} else {
		_ = ucli.ShowCommandHelp(context.Background(), cmd.Lineage()[1], cmd.Name)
	}
	return ucli.Exit("", 2)
}
