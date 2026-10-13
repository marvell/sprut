package cli

import (
	"context"
	"fmt"
	"io"

	ucli "github.com/urfave/cli/v3"
)

func logoutCommand(env []string, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:      "logout",
		Usage:     "delete the credentials of an OAuth upstream",
		Arguments: []ucli.Argument{&ucli.StringArg{Name: "upstream", UsageText: "<upstream>", Required: true}},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			name := cmd.StringArg("upstream")
			a, err := upstreamAuth(cmd, env, newLogger(stderr, false))
			if err != nil {
				return err
			}
			had, err := a.Logout(ctx)
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
