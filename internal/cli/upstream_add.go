package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
)

// addCommand is upstream add. args are sprut's whole command line, since
// urfave/cli drops the "--" that tells the stdio form from the HTTP one.
func addCommand(args, env []string, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:  "add",
		Usage: "add an Upstream to the Config, creating the Config if there is none",
		UsageText: "sprut upstream add [options] <name> -- <command> [args...]\n" +
			"sprut upstream add [options] <name> <http(s) url>",
		Arguments: []ucli.Argument{
			&ucli.StringArg{Name: "name", UsageText: "<name>", Required: true},
			&ucli.StringArgs{Name: "target", UsageText: "(-- <command> [args...] | <url>)", Max: -1},
		},
		Flags: []ucli.Flag{
			&ucli.BoolFlag{Name: "force", Usage: "replace an Upstream of that name, keeping its place in the Config"},
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			name := cmd.StringArg("name")
			def, err := definition(args, cmd.StringArgs("target"))
			if err != nil {
				return usageError(ctx, cmd, err)
			}
			err = editConfig(cmd.String("config"), config.LookupIn(env), stderr, name, func(doc *config.Document) error {
				return doc.Add(name, def, cmd.Bool("force"))
			})
			if errors.Is(err, config.ErrExists) {
				return fmt.Errorf("%w; use --force to replace it", err)
			}
			if err != nil {
				return err
			}
			changed(stdout, stderr, "added upstream %q", name)
			return nil
		},
	}
}

// definition is the Upstream that target, the arguments after add's name,
// defines: the command and args after a "--" that comes right after the
// name, or else a single http(s) URL. args is the whole command line.
func definition(args, target []string) (config.Definition, error) {
	if i := slices.Index(args, "--"); i >= 0 {
		tail := args[i+1:]
		if len(tail) == 0 || !slices.Equal(target, tail) {
			return config.Definition{}, errors.New(`"--" must come right after the name and be followed by a command`)
		}
		return config.Definition{Command: tail[0], Args: tail[1:]}, nil
	}
	if len(target) == 0 {
		return config.Definition{}, errors.New("missing the upstream's command or URL")
	}
	if len(target) > 1 || !isHTTPURL(target[0]) {
		return config.Definition{}, fmt.Errorf("%q is neither -- and a command nor an http(s) URL", strings.Join(target, " "))
	}
	return config.Definition{URL: target[0]}, nil
}

// isHTTPURL reports whether s is an http(s) URL, taking the scheme in any
// case, as URLs do.
func isHTTPURL(s string) bool {
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// editConfig applies edit to the Config that flag and the environment
// point at, and writes it back if the result parses, printing the
// warnings about the Upstream name to stderr. It refuses to edit a Config
// that already fails to parse. A missing Config is created.
func editConfig(flag string, lookupEnv config.LookupEnv, stderr io.Writer, name string, edit func(*config.Document) error) error {
	path, _, data, err := readConfig(flag, lookupEnv)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data = []byte("{}")
	case err != nil:
		return err
	default:
		if _, _, err := config.ParseEntries(data, lookupEnv); err != nil {
			return fmt.Errorf("config %s: %w", path, err)
		}
	}
	doc, err := config.ParseDocument(data)
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if err := edit(doc); err != nil {
		return err
	}
	out := doc.Bytes()
	_, warnings, err := config.ParseEntries(out, lookupEnv)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		if w.Upstream == name {
			_, _ = fmt.Fprintf(stderr, "warning: upstream %q: %s\n", name, w.Message)
		}
	}
	if err := config.WriteFile(path, out); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// changed reports a change to the Config: what changed on stdout, and on
// stderr that running Agents don't see it yet.
func changed(stdout, stderr io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(stdout, format+"\n", a...)
	_, _ = fmt.Fprintln(stderr, "note: running Agents see this change only after their sprut restarts")
}
