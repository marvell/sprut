package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

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
			c, err := openConfig(cmd.String("config"), config.LookupIn(env), log)
			if err != nil {
				return err
			}
			// A disabled Upstream too, so that it can be enabled logged in.
			u, err := c.Upstream(name)
			if err != nil {
				return err
			}
			if err := openAuths(env).For(u).Login(ctx, !cmd.Bool("no-browser"), stderr, version(), log); err != nil {
				return fmt.Errorf("upstream %q: %w", name, err)
			}
			return nil
		},
	}
}

// openAuths returns the Logins and Credentials of every Upstream, in the
// Store that env points at.
func openAuths(env []string) *gateway.Auths {
	return gateway.OpenAuths(env, loginRemedy)
}

// loginRemedy is what the user runs to log in to u: the login command
// above, with -c unless u's Config is where sprut looks by default.
func loginRemedy(u config.Upstream) string {
	cmd := "run: sprut upstream login "
	if u.ConfigPath != "" {
		cmd += "-c " + shellQuote(u.ConfigPath) + " "
	}
	return cmd + u.Name // a valid name needs no quoting
}

// shellQuote quotes s for a POSIX shell, unless it needs no quoting.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:@%+=,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
