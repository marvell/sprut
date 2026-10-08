package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
	"github.com/marvell/sprut/internal/gateway"
)

func authCommand(env []string, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:  "auth",
		Usage: "manage the Credentials of OAuth Upstreams",
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			return usageError(ctx, cmd, nil)
		},
		Commands: []*ucli.Command{
			loginCommand(env, stderr),
			logoutCommand(env, stdout, stderr),
			statusCommand(env, stdout, stderr),
		},
	}
}

func loginCommand(env []string, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name: "login",
		Usage: "log in to OAuth Upstreams and store their Credentials; " +
			"without names, to every one in the Config whose Credentials are missing or dead",
		Arguments: []ucli.Argument{&ucli.StringArgs{Name: "upstream", UsageText: "[upstream...]", Max: -1}},
		Flags: []ucli.Flag{
			configFlag(),
			&ucli.BoolFlag{Name: "no-browser", Usage: "only print the authorization URL, without opening a browser"},
			&ucli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "log at DEBUG level"},
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			log := newLogger(stderr, cmd.Bool("verbose"))
			upstreams, err := loadConfig(cmd.String("config"), config.LookupIn(env), log)
			if err != nil {
				return err
			}
			names := cmd.StringArgs("upstream")
			var todo []config.Upstream
			for _, name := range names {
				u, err := findUpstream(upstreams, name)
				if err != nil {
					return err
				}
				todo = append(todo, u)
			}
			failed := false
			if len(names) == 0 {
				todo, failed = needingLogin(ctx, upstreams, env, stderr, log)
			}
			for _, u := range todo {
				if err := gateway.Login(ctx, u, env, !cmd.Bool("no-browser"), stderr, version(), log); err != nil {
					_, _ = fmt.Fprintf(stderr, "sprut: upstream %q: %v\n", u.Name, err)
					failed = true
				}
			}
			if failed {
				return ucli.Exit("", 1)
			}
			return nil
		},
	}
}

// needingLogin returns the OAuth Upstreams among upstreams that need a
// Login, checking them all at once, and whether checking any failed. It
// reports on stderr, in Config order, each that needs none and each that
// could not be checked.
func needingLogin(ctx context.Context, upstreams []config.Upstream, env []string, stderr io.Writer, log *slog.Logger) ([]config.Upstream, bool) {
	type probe struct {
		needed bool
		err    error
	}
	probes := make([]*probe, len(upstreams))
	var wg sync.WaitGroup
	for i, u := range upstreams {
		if gateway.CheckOAuthUpstream(u) != nil {
			continue
		}
		p := &probe{}
		probes[i] = p
		wg.Go(func() { p.needed, p.err = gateway.NeedsLogin(ctx, u, env, version(), log) })
	}
	wg.Wait()

	var todo []config.Upstream
	failed := false
	for i, p := range probes {
		switch {
		case p == nil:
		case p.err != nil:
			_, _ = fmt.Fprintf(stderr, "sprut: upstream %q: %v\n", upstreams[i].Name, p.err)
			failed = true
		case p.needed:
			todo = append(todo, upstreams[i])
		default:
			_, _ = fmt.Fprintf(stderr, "%s: no login needed\n", upstreams[i].Name)
		}
	}
	return todo, failed
}

func logoutCommand(env []string, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:      "logout",
		Usage:     "delete the Credentials of an OAuth Upstream",
		Arguments: []ucli.Argument{&ucli.StringArg{Name: "upstream", UsageText: "<upstream>", Required: true}},
		Flags:     []ucli.Flag{configFlag()},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			name := cmd.StringArg("upstream")
			upstreams, err := loadConfig(cmd.String("config"), config.LookupIn(env), newLogger(stderr, false))
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

func statusCommand(env []string, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:  "status",
		Usage: "list each OAuth Upstream with the state of its Credentials",
		Flags: []ucli.Flag{configFlag()},
		Action: func(_ context.Context, cmd *ucli.Command) error {
			upstreams, err := loadConfig(cmd.String("config"), config.LookupIn(env), newLogger(stderr, false))
			if err != nil {
				return err
			}
			store, err := credentials.NewStore(config.LookupIn(env))
			if err != nil {
				return err
			}
			listed := 0
			for _, u := range upstreams {
				if gateway.CheckOAuthUpstream(u) != nil {
					continue
				}
				status, err := gateway.Status(store, u)
				if err != nil {
					return fmt.Errorf("upstream %q: %w", u.Name, err)
				}
				_, _ = fmt.Fprintf(stdout, "%s: %s\n", u.Name, status)
				listed++
			}
			if listed == 0 {
				_, _ = fmt.Fprintln(stdout, "no OAuth upstreams in the config")
			}
			return nil
		},
	}
}
