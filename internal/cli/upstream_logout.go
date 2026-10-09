package cli

import (
	"context"
	"fmt"
	"io"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
	"github.com/marvell/sprut/internal/gateway"
)

func logoutCommand(env []string, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:      "logout",
		Usage:     "delete the credentials of an OAuth upstream",
		Arguments: []ucli.Argument{&ucli.StringArg{Name: "upstream", UsageText: "<upstream>", Required: true}},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			name := cmd.StringArg("upstream")
			lookupEnv := config.LookupIn(env)
			upstreams, _, err := loadConfig(cmd.String("config"), lookupEnv, newLogger(stderr, false))
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
			store, err := credentials.NewStore(lookupEnv)
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
