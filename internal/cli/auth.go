package cli

import (
	"context"
	"fmt"
	"io"
	"slices"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/gateway"
)

func authCommand(env []string, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:  "auth",
		Usage: "manage the Credentials of OAuth Upstreams",
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			return usageError(ctx, cmd, nil)
		},
		Commands: []*ucli.Command{
			loginCommand(env, stderr),
		},
	}
}

func loginCommand(env []string, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:      "login",
		Usage:     "log in to an OAuth Upstream and store its Credentials",
		Arguments: []ucli.Argument{&ucli.StringArg{Name: "upstream", UsageText: "<upstream>", Required: true}},
		Flags: []ucli.Flag{
			configFlag(),
			&ucli.BoolFlag{Name: "no-browser", Usage: "only print the authorization URL, without opening a browser"},
			&ucli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "log at DEBUG level"},
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			name := cmd.StringArg("upstream")
			log := newLogger(stderr, cmd.Bool("verbose"))
			upstreams, err := loadConfig(cmd.String("config"), config.LookupIn(env), log)
			if err != nil {
				return err
			}
			i := slices.IndexFunc(upstreams, func(u config.Upstream) bool { return u.Name == name })
			if i < 0 {
				return fmt.Errorf("no upstream %q in the config", name)
			}
			// Opening a browser is not supported yet, so --no-browser is
			// what every Login does.
			if err := gateway.Login(ctx, upstreams[i], env, stderr, version(), log); err != nil {
				return fmt.Errorf("upstream %q: %w", name, err)
			}
			return nil
		},
	}
}
