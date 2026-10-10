package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"slices"
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
			entries, _, _, err := loadEntries(cmd.String("config"), lookupEnv)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				return nil
			}
			store, err := credentials.NewStore(lookupEnv)
			if err != nil {
				return err
			}
			states := make([]string, len(entries))
			for i, e := range entries {
				states[i] = state(e)
			}
			healthy := true
			if cmd.Bool("check") {
				healthy = checkStates(ctx, entries, states, env, cmd.Duration("startup-timeout"))
			}
			w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tTRANSPORT\tTARGET\tSTATE\tAUTH")
			for i, e := range entries {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Name, orDash(string(e.Transport)), orDash(target(e)), states[i], authState(store, e))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if !healthy {
				return ucli.Exit("", 1)
			}
			return nil
		},
	}
}

// checkStates starts every enabled entry of entries, as serve would, sets
// its state in states to how it fared, and reports whether all of them
// work.
func checkStates(ctx context.Context, entries []config.Entry, states []string, env []string, startupTimeout time.Duration) bool {
	upstreams := config.Started(entries)
	// By name, which is unique in a Config.
	health := make(map[string]gateway.Health, len(upstreams))
	for i, h := range check(ctx, upstreams, env, startupTimeout) {
		health[upstreams[i].Name] = h
	}
	healthy := true
	for i, e := range entries {
		if h, ok := health[e.Name]; ok {
			healthy = healthy && h.Err == nil
			states[i] = healthState(h)
		}
	}
	return healthy
}

// check is gateway.Check with its failures not logged: the caller reports
// them.
func check(ctx context.Context, upstreams []config.Upstream, env []string, startupTimeout time.Duration) []gateway.Health {
	return gateway.Check(ctx, upstreams, env, startupTimeout, version(), slog.New(slog.DiscardHandler))
}

// healthState is how h fared, as list --check shows it in STATE.
func healthState(h gateway.Health) string {
	switch {
	case h.Err == nil:
		return fmt.Sprintf("ok (%d tools)", h.Tools)
	case h.NeedsLogin():
		return "needs login"
	}
	return "failed: " + oneLine(h.Err.Error())
}

// noCheckFlag is the --no-check of the commands that check the Upstream
// they write.
func noCheckFlag() *ucli.BoolFlag {
	return &ucli.BoolFlag{Name: "no-check", Usage: "don't start the upstream to check that it works"}
}

// checkWritten starts the Upstream called name, just written to the Config
// that cmd's --config and env point at, as list --check would, unless
// --no-check, and says how it fared: on stdout if it works, else on stderr
// with the remedy, if any. It checks nothing the Gateway wouldn't start.
// Whatever it finds, the Config stays written, so nothing it finds is an
// error.
func checkWritten(ctx context.Context, cmd *ucli.Command, env []string, name string, stdout, stderr io.Writer) {
	if cmd.Bool("no-check") {
		return
	}
	// The edit has already printed the warnings about the Upstream.
	u, err := loadUpstream(cmd.String("config"), config.LookupIn(env), slog.New(slog.DiscardHandler), name)
	if errors.Is(err, config.ErrNotFound) {
		return
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: upstream %q: not checked: %v\n", name, err)
		return
	}
	h := check(ctx, []config.Upstream{u}, env, cmd.Duration("startup-timeout"))[0]
	state, hint := healthState(h), h.Hint()
	switch {
	case h.Err == nil:
		_, _ = fmt.Fprintf(stdout, "upstream %q: %s\n", name, state)
	case hint != "":
		_, _ = fmt.Fprintf(stderr, "upstream %q: %s; %s\n", name, state, hint)
	default:
		_, _ = fmt.Fprintf(stderr, "warning: upstream %q: %s\n", name, state)
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

// state is whether the Gateway starts the entry e, or why not.
func state(e config.Entry) string {
	switch {
	case e.Disabled:
		return "disabled"
	case e.Skipped != "":
		return "skipped: " + e.Skipped
	}
	return "enabled"
}

// authState describes the Credentials of the entry e, or is "-" if it is
// not an OAuth Upstream, or can't be told to be one: a skipped entry has no
// URL to bind Credentials to. Credentials that can't be read are one
// entry's problem, so they don't fail the list.
func authState(store *credentials.Store, e config.Entry) string {
	if e.Upstream == nil || gateway.CheckOAuthUpstream(*e.Upstream) != nil {
		return "-"
	}
	status, err := gateway.Status(store, *e.Upstream)
	if err != nil {
		return "error: " + err.Error()
	}
	return status
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// editConfig applies edit to the Config that flag and the environment
// point at, and writes it back if the result parses, printing the
// warnings about the Upstream name to stderr. It refuses to edit a Config
// that already fails to parse. A missing Config is created. edit reports
// whether it changed the Config; if not, the file is left as it is.
func editConfig(flag string, lookupEnv config.LookupEnv, stderr io.Writer, name string, edit func(*config.Document) (bool, error)) error {
	path, _, data, err := readConfig(flag, lookupEnv)
	if errors.Is(err, fs.ErrNotExist) {
		data = []byte("{}")
	} else if err != nil {
		return err
	}
	doc, _, _, err := parseConfig(path, data, lookupEnv)
	if err != nil {
		return err
	}
	edited, err := edit(doc)
	if err != nil || !edited {
		return err
	}
	_, warnings, err := doc.Entries(lookupEnv)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		if w.Upstream == name {
			_, _ = fmt.Fprintf(stderr, "warning: upstream %q: %s\n", name, w.Message)
		}
	}
	if err := config.WriteFile(path, doc.Bytes()); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// changed reports a change to the Config: what changed on stdout, and on
// stderr that running Agents don't see it yet.
func changed(stdout, stderr io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(stdout, format+"\n", a...)
	_, _ = fmt.Fprintln(stderr, "note: running agents see this change only after their sprut restarts")
}

// loadUpstream loads the Config as loadConfig does, and returns the
// Upstream called name among those it starts.
func loadUpstream(flag string, lookupEnv config.LookupEnv, log *slog.Logger, name string) (config.Upstream, error) {
	upstreams, _, err := loadConfig(flag, lookupEnv, log)
	if err != nil {
		return config.Upstream{}, err
	}
	i := slices.IndexFunc(upstreams, func(u config.Upstream) bool { return u.Name == name })
	if i < 0 {
		return config.Upstream{}, fmt.Errorf("upstream %q: %w", name, config.ErrNotFound)
	}
	return upstreams[i], nil
}
