package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"text/tabwriter"
	"time"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
	"github.com/marvell/sprut/internal/gateway"
)

func upstreamCommand(args, env []string, stdout, stderr io.Writer) *ucli.Command {
	commands := []*ucli.Command{
		addCommand(args, env, stdout, stderr),
		removeCommand(env, stdout, stderr),
		switchCommand(false, env, stdout, stderr),
		switchCommand(true, env, stdout, stderr),
		listCommand(env, stdout),
		loginCommand(env, stderr),
		logoutCommand(env, stdout, stderr),
	}
	// h and help are valid Upstream names, and urfave/cli's help
	// subcommand would take them. Every leaf drops it, list too, so that
	// none reads h or help differently; --help and -h stay.
	for _, c := range commands {
		c.HideHelpCommand = true
	}
	return &ucli.Command{
		Name:  "upstream",
		Usage: "manage the upstreams in the config",
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			return usageError(ctx, cmd, nil)
		},
		Commands: commands,
	}
}

func listCommand(env []string, stdout io.Writer) *ucli.Command {
	return &ucli.Command{
		Name: "list",
		Usage: "list every upstream in the config with its state and the state of its credentials; " +
			"starts no upstream and contacts no server, unless --check",
		Flags: []ucli.Flag{
			&ucli.BoolFlag{Name: "check", Usage: "start every enabled upstream, as serve would, and say whether it works; exit 1 if any does not"},
			startupTimeoutFlag(),
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			lookupEnv := config.LookupIn(env)
			c, err := config.Open(cmd.String("config"), lookupEnv)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if len(c.Entries) == 0 {
				return nil
			}
			store, err := credentials.NewStore(lookupEnv)
			if err != nil {
				return err
			}
			var check *gateway.CheckOptions
			if cmd.Bool("check") {
				check = checkOptions(cmd, env)
			}
			reports := gateway.Reports(ctx, c.Entries, store, check)
			w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tTRANSPORT\tTARGET\tSTATE\tAUTH")
			for _, r := range reports {
				e := r.Entry
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Name, orDash(string(e.Transport)), orDash(target(e)), state(r), auth(r.Auth))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if !gateway.AllWork(reports) {
				return ucli.Exit("", 1)
			}
			return nil
		},
	}
}

// checkOptions are how cmd checks Upstreams, with their failures not
// logged: the caller reports them.
func checkOptions(cmd *ucli.Command, env []string) *gateway.CheckOptions {
	return &gateway.CheckOptions{
		Env:            env,
		StartupTimeout: cmd.Duration("startup-timeout"),
		Version:        version(),
		Log:            slog.New(slog.DiscardHandler),
	}
}

// health is how h fared, as list --check shows it in STATE.
func health(h *gateway.Health) string {
	switch h.State {
	case gateway.HealthWorks:
		return fmt.Sprintf("ok (%d tools)", h.Tools)
	case gateway.HealthNeedsLogin:
		return "needs login"
	}
	return "failed: " + oneLine(h.Err.Error())
}

// noCheckFlag is the --no-check of the commands that check the Upstream
// they write.
func noCheckFlag() *ucli.BoolFlag {
	return &ucli.BoolFlag{Name: "no-check", Usage: "don't start the upstream to check that it works"}
}

// checkWritten starts the Upstream called name in c, the Config just
// written, as list --check would, unless cmd has --no-check, and says how
// it fared: on stdout if it works, else on stderr with the remedy, if any.
// It checks nothing the Gateway wouldn't start. Whatever it finds, the
// Config stays written, so nothing it finds is an error.
func checkWritten(ctx context.Context, cmd *ucli.Command, c *config.Config, env []string, name string, stdout, stderr io.Writer) {
	if cmd.Bool("no-check") {
		return
	}
	e, err := c.Lookup(name)
	if err != nil {
		return
	}
	// Only the Health is shown, so Credentials that can't be located don't
	// matter here; the check reports them itself.
	store, _ := credentials.NewStore(config.LookupIn(env))
	h := gateway.Reports(ctx, []config.Entry{e}, store, checkOptions(cmd, env))[0].Health
	switch {
	case h == nil:
	case h.State == gateway.HealthWorks:
		_, _ = fmt.Fprintf(stdout, "upstream %q: %s\n", name, health(h))
	case h.Remedy != "":
		_, _ = fmt.Fprintf(stderr, "upstream %q: %s; %s\n", name, health(h), h.Remedy)
	default:
		_, _ = fmt.Fprintf(stderr, "warning: upstream %q: %s\n", name, health(h))
	}
}

// oneLine is s with every run of whitespace, line breaks included, made
// one space, so that it fits in a table cell.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// target is what the entry e runs or connects to, as the Config writes it.
func target(e config.Entry) string {
	if e.Transport == config.Stdio {
		return strings.Join(append([]string{e.Command}, e.Args...), " ")
	}
	return e.URL
}

// state is what list shows in STATE for r: how it fared if checked, else
// whether the Gateway starts it, or why not.
func state(r gateway.Report) string {
	if r.Health != nil {
		return health(r.Health)
	}
	switch r.Start {
	case gateway.StartDisabled:
		return "disabled"
	case gateway.StartSkipped:
		return "skipped: " + r.Entry.Skipped
	}
	return "enabled"
}

// auth is what list shows in AUTH for a: the state of the Credentials, or
// "-" where OAuth doesn't apply. Credentials that can't be read are one
// entry's problem, so they don't fail the list.
func auth(a gateway.Auth) string {
	switch a.State {
	case gateway.AuthNotOAuth:
		return "-"
	case gateway.AuthNoCredentials:
		return "no credentials"
	case gateway.AuthDead:
		return a.Reason + "; " + a.Remedy
	case gateway.AuthUnreadable:
		return "error: " + a.Err.Error()
	}
	s := "logged in, access token "
	switch {
	case a.Expiry.IsZero():
		s += "does not expire"
	case a.Expired:
		s += "expired " + a.Expiry.Local().Format(time.RFC3339)
	default:
		s += "expires " + a.Expiry.Local().Format(time.RFC3339)
	}
	if !a.Renewable {
		return s + ", not renewable"
	}
	return s + ", renewable"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// warn prints to stderr the warnings c has about the Upstream name, just
// written.
func warn(stderr io.Writer, c *config.Config, name string) {
	for _, w := range c.Warnings {
		if w.Upstream == name {
			_, _ = fmt.Fprintf(stderr, "warning: upstream %q: %s\n", name, w.Message)
		}
	}
}

// changed reports a change to the Config: what changed on stdout, and on
// stderr that running Agents don't see it yet.
func changed(stdout, stderr io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(stdout, format+"\n", a...)
	_, _ = fmt.Fprintln(stderr, "note: running agents see this change only after their sprut restarts")
}
