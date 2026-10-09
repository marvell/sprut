package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"text/tabwriter"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
	"github.com/marvell/sprut/internal/gateway"
)

func upstreamCommand(env []string, stdout io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:  "upstream",
		Usage: "manage the Upstreams in the Config",
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			return usageError(ctx, cmd, nil)
		},
		Commands: []*ucli.Command{
			listCommand(env, stdout),
		},
	}
}

func listCommand(env []string, stdout io.Writer) *ucli.Command {
	return &ucli.Command{
		Name: "list",
		Usage: "list every Upstream in the Config with its state and the state of its Credentials; " +
			"starts no Upstream and contacts no server",
		Action: func(_ context.Context, cmd *ucli.Command) error {
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
			w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tTRANSPORT\tTARGET\tSTATE\tAUTH")
			for _, e := range entries {
				auth, err := authState(store, e)
				if err != nil {
					return fmt.Errorf("upstream %q: %w", e.Name, err)
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Name, orDash(string(e.Transport)), orDash(target(e)), state(e), auth)
			}
			return w.Flush()
		},
	}
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
// not an OAuth Upstream, or can't be told to be one.
func authState(store *credentials.Store, e config.Entry) (string, error) {
	if e.Upstream == nil || gateway.CheckOAuthUpstream(*e.Upstream) != nil {
		return "-", nil
	}
	return gateway.Status(store, *e.Upstream)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
