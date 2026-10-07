package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
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
			&ucli.StringFlag{Name: "config", Aliases: []string{"c"}, Usage: "path to the Config file"},
			&ucli.DurationFlag{Name: "startup-timeout", Value: 30 * time.Second, Usage: "how long each Upstream may take to start"},
			&ucli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "log at DEBUG level: every Namespaced tool and tool call"},
			&ucli.BoolFlag{Name: "dry-run", Usage: "start every Upstream as usual, then shut down; exit 1 if any failed"},
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			// Checked here rather than by a flag Validator, which urfave/cli
			// reports with help on stdout and exit code 1.
			startupTimeout := cmd.Duration("startup-timeout")
			if startupTimeout <= 0 {
				return ucli.Exit(fmt.Sprintf("--startup-timeout must be positive, got %s", startupTimeout), 2)
			}
			log := newLogger(stderr, cmd.Bool("verbose"))
			upstreams, err := loadConfig(cmd.String("config"), lookupIn(env), log)
			if err != nil {
				return err
			}

			gw := gateway.Start(ctx, upstreams, env, startupTimeout, version(), log)
			defer gw.Close()

			if cmd.Bool("dry-run") {
				if gw.Failed() > 0 {
					return ucli.Exit("", 1)
				}
				return nil
			}

			// The Agent going away (EOF on stdin) or ctx being cancelled by a
			// signal are both normal ends of a session.
			if err := gw.Serve(ctx, stdin, stdout); err != nil && ctx.Err() == nil {
				log.Info("agent session ended", "err", err)
			}
			return nil
		},
	}
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
// its warnings and returns the Upstreams to start.
func loadConfig(flag string, lookupEnv config.LookupEnv, log *slog.Logger) ([]config.Upstream, error) {
	path, err := config.Path(flag, lookupEnv)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	upstreams, warnings, err := config.Parse(data, lookupEnv)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	for _, w := range warnings {
		log.Warn(w.Message, "upstream", w.Upstream)
	}
	return upstreams, nil
}

// lookupIn returns a lookup over env, which is in os.Environ form. As with
// a real environment, the last entry for a key wins.
func lookupIn(env []string) config.LookupEnv {
	vars := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}
