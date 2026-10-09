package cli

import (
	"context"
	"fmt"
	"io"
	"slices"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
	"github.com/marvell/sprut/internal/gateway"
)

func loginCommand(env []string, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:      "login",
		Usage:     "log in to an OAuth Upstream and store its Credentials",
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

func logoutCommand(env []string, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:      "logout",
		Usage:     "delete the Credentials of an OAuth Upstream",
		Arguments: []ucli.Argument{&ucli.StringArg{Name: "upstream", UsageText: "<upstream>", Required: true}},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			name := cmd.StringArg("upstream")
			upstreams, _, err := loadConfig(cmd.String("config"), config.LookupIn(env), newLogger(stderr, false))
			if err != nil {
				return err
			}
			u, err := findUpstream(upstreams, name)
			if err != nil {
				return err
			}
			if err := gateway.CheckOAuthUpstream(u); err != nil {
				return fmt.Errorf("upstream %q: %w", name, err)
			}
			store, err := credentials.NewStore(config.LookupIn(env))
			if err != nil {
				return err
			}
			had, err := store.Delete(ctx, name)
			if err != nil {
				return fmt.Errorf("upstream %q: %w", name, err)
			}
			if had {
				_, _ = fmt.Fprintf(stdout, "%s: logged out\n", name)
			} else {
				_, _ = fmt.Fprintf(stdout, "%s: no credentials\n", name)
			}
			return nil
		},
	}
}

// findUpstream returns the Upstream called name.
func findUpstream(upstreams []config.Upstream, name string) (config.Upstream, error) {
	i := slices.IndexFunc(upstreams, func(u config.Upstream) bool { return u.Name == name })
	if i < 0 {
		return config.Upstream{}, fmt.Errorf("no upstream %q in the config", name)
	}
	return upstreams[i], nil
}
