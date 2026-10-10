package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/gateway"
)

func serveCommand(env []string, stdin io.Reader, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:  "serve",
		Usage: "run the gateway as a stdio MCP server",
		Flags: []ucli.Flag{
			startupTimeoutFlag(),
			&ucli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "log at DEBUG level: every namespaced tool and tool call"},
			&ucli.BoolFlag{Name: "dry-run", Usage: "start every upstream as usual, then shut down; exit 1 if any failed"},
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			startupTimeout := cmd.Duration("startup-timeout")
			log := newLogger(stderr, cmd.Bool("verbose"))
			c, err := openConfig(cmd.String("config"), config.LookupIn(env), log)
			if err != nil {
				return err
			}
			upstreams := c.Upstreams()
			log.Info("config loaded", "path", c.Path, "upstreams", len(upstreams))

			gw := gateway.Start(ctx, upstreams, env, startupTimeout, version(), log)
			defer gw.Close()

			if cmd.Bool("dry-run") {
				if gw.SkippedCount() > 0 {
					return ucli.Exit("", 1)
				}
				return nil
			}

			// The Agent going away (EOF on stdin, which Serve reports as nil)
			// or ctx being cancelled by a signal are both normal ends of a
			// session; anything else, such as a failed read, is not.
			if err := gw.Serve(ctx, stdin, stdout); err != nil && ctx.Err() == nil {
				return fmt.Errorf("serving the agent: %w", err)
			}
			return nil
		},
	}
}

func startupTimeoutFlag() *ucli.DurationFlag {
	return &ucli.DurationFlag{
		Name:  "startup-timeout",
		Value: 30 * time.Second,
		Usage: "how long each upstream may take to start",
		Validator: func(d time.Duration) error {
			if d <= 0 {
				return errors.New("must be positive")
			}
			return nil
		},
	}
}

func configFlag() *ucli.StringFlag {
	return &ucli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "path to the config file"}
}

// newLogger logs logfmt to w at INFO, or at DEBUG when verbose.
func newLogger(w io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// openConfig opens the Config that flag and lookupEnv point at, and logs
// its warnings.
func openConfig(flag string, lookupEnv config.LookupEnv, log *slog.Logger) (*config.Config, error) {
	c, err := config.Open(flag, lookupEnv)
	if err != nil {
		return nil, err
	}
	for _, w := range c.Warnings {
		log.Warn(w.Message, "upstream", w.Upstream)
	}
	return c, nil
}
