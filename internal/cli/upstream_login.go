package cli

import (
	"context"
	"fmt"
	"io"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/gateway"
)

func loginCommand(env []string, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:      "login",
		Usage:     "log in to an OAuth upstream and store its credentials",
		Arguments: []ucli.Argument{&ucli.StringArg{Name: "upstream", UsageText: "<upstream>", Required: true}},
		Flags: []ucli.Flag{
			&ucli.BoolFlag{Name: "no-browser", Usage: "only print the authorization URL, without opening a browser"},
			&ucli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "log at DEBUG level"},
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			name := cmd.StringArg("upstream")
			log := newLogger(stderr, cmd.Bool("verbose"))
			upstreams, _, err := loadConfig(cmd.String("config"), config.LookupIn(env), log)
			if err != nil {
				return err
			}
			u, err := findUpstream(upstreams, name)
			if err != nil {
				return err
			}
			if err := gateway.Login(ctx, u, env, !cmd.Bool("no-browser"), stderr, version(), log); err != nil {
				return fmt.Errorf("upstream %q: %w", name, err)
			}
			return nil
		},
	}
}
