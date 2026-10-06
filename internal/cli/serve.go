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
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			log := slog.New(slog.NewTextHandler(stderr, nil))

			lookupEnv := lookupIn(env)
			path, err := config.Path(cmd.String("config"), lookupEnv)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading config: %w", err)
			}
			upstreams, warnings, err := config.Parse(data, lookupEnv)
			if err != nil {
				return fmt.Errorf("config %s: %w", path, err)
			}
			for _, w := range warnings {
				log.Warn(w.Message, "upstream", w.Upstream)
			}

			gw := gateway.Start(ctx, upstreams, env, cmd.Duration("startup-timeout"), version(), log)
			defer gw.Close()

			// The Agent going away (EOF on stdin) or ctx being cancelled by a
			// signal are both normal ends of a session.
			if err := gw.Serve(ctx, stdin, stdout); err != nil && ctx.Err() == nil {
				log.Info("agent session ended", "err", err)
			}
			return nil
		},
	}
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
