package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

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
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			log := slog.New(slog.NewTextHandler(stderr, nil))

			path := cmd.String("config")
			if path == "" {
				return errors.New("no Config given, use -c PATH")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading config: %w", err)
			}
			upstreams, err := config.Parse(data)
			if err != nil {
				return fmt.Errorf("config %s: %w", path, err)
			}

			gw, err := gateway.Start(ctx, upstreams, env, version(), log)
			if err != nil {
				return err
			}
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
