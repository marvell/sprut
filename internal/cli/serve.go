package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/gateway"
)

func serveCommand(env []string, stdin io.Reader, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:  "serve",
		Usage: "run the Gateway as a stdio MCP server",
		Flags: []ucli.Flag{
			&ucli.DurationFlag{
				Name:  "startup-timeout",
				Value: 30 * time.Second,
				Usage: "how long each Upstream may take to start",
				Validator: func(d time.Duration) error {
					if d <= 0 {
						return errors.New("must be positive")
					}
					return nil
				},
			},
			&ucli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "log at DEBUG level: every Namespaced tool and tool call"},
			&ucli.BoolFlag{Name: "dry-run", Usage: "start every Upstream as usual, then shut down; exit 1 if any failed"},
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			startupTimeout := cmd.Duration("startup-timeout")
			log := newLogger(stderr, cmd.Bool("verbose"))
			upstreams, path, err := loadConfig(cmd.String("config"), config.LookupIn(env), log)
			if err != nil {
				return err
			}
			log.Info("config loaded", "path", path, "upstreams", len(upstreams))

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

func configFlag() *ucli.StringFlag {
	return &ucli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "path to the Config file"}
}

// newLogger logs logfmt to w at INFO, or at DEBUG when verbose.
func newLogger(w io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// loadConfig reads the Config that flag and the environment point at, logs
// its warnings and returns the Upstreams to start and the Config's absolute
// path. Unless the Config is at
// the default location, each Upstream carries its path as ConfigPath: the
// Agent may run sprut with -c or $SPRUT_CONFIG that the user's shell lacks,
// and in another directory.
func loadConfig(flag string, lookupEnv config.LookupEnv, log *slog.Logger) ([]config.Upstream, string, error) {
	path, named, err := config.Path(flag, lookupEnv)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("reading config: %w", err)
	}
	upstreams, warnings, err := config.Parse(data, lookupEnv)
	if err != nil {
		return nil, "", fmt.Errorf("config %s: %w", path, err)
	}
	for _, w := range warnings {
		log.Warn(w.Message, "upstream", w.Upstream)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("config %s: %w", path, err)
	}
	if named {
		for i := range upstreams {
			upstreams[i].ConfigPath = abs
		}
	}
	return upstreams, abs, nil
}
