package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		Usage: "add an upstream to the config, creating the config if there is none",
		UsageText: "sprut upstream add [options] <name> -- <command> [args...]\n" +
			"sprut upstream add [options] <name> <http(s) url>",
		Arguments: []ucli.Argument{
			&ucli.StringArg{Name: "name", UsageText: "<name>", Required: true},
			&ucli.StringArgs{Name: "target", UsageText: "(-- <command> [args...] | <url>)", Max: -1},
		},
		Flags: []ucli.Flag{
			&ucli.StringSliceFlag{
				Name: "env", Aliases: []string{"e"}, Usage: "set an environment variable of a stdio upstream, as KEY=VALUE, such as -e 'TOKEN=${TOKEN}'",
			},
			&ucli.StringSliceFlag{
				Name: "header", Aliases: []string{"H"}, Usage: "set a header of an HTTP upstream, as 'Name: value', such as -H 'Authorization: Bearer ${TOKEN}'",
			},
			&ucli.StringFlag{Name: "oauth-client-id", Usage: "log in to an HTTP upstream as this pre-registered OAuth client"},
			&ucli.StringFlag{
				Name: "oauth-client-secret", Usage: "the secret of the --oauth-client-id client, such as '${CLIENT_SECRET}'",
			},
			&ucli.StringSliceFlag{Name: "oauth-scope", Usage: "request this OAuth scope in place of those the HTTP upstream asks for"},
			&ucli.BoolFlag{Name: "disabled", Usage: "add the upstream switched off"},
			&ucli.BoolFlag{Name: "force", Usage: "replace an upstream of that name, keeping its place in the config"},
			noCheckFlag(),
			startupTimeoutFlag(),
		},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			name := cmd.StringArg("name")
			def, err := definition(args, cmd.StringArgs("target"))
			if err != nil {
				return usageError(ctx, cmd, err)
			}
			if err := setFields(cmd, &def); err != nil {
				return usageError(ctx, cmd, err)
			}
			err = editConfig(cmd.String("config"), config.LookupIn(env), stderr, name, func(doc *config.Document) (bool, error) {
				return true, doc.Add(name, def, cmd.Bool("force"))
			})
			if errors.Is(err, config.ErrExists) {
				return fmt.Errorf("%w; use --force to replace it", err)
			}
			if err != nil {
				return err
			}
			changed(stdout, stderr, "added upstream %q", name)
			checkWritten(ctx, cmd, env, name, stdout, stderr)
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

// transportFlags are add's flags that set a field of only one Transport's
// Upstreams.
var transportFlags = []struct {
	name      string
	usage     string // the flag as errors name it
	transport config.Transport
}{
	{"env", "-e/--env", config.Stdio},
	{"header", "-H/--header", config.HTTP},
	{"oauth-client-id", "--oauth-client-id", config.HTTP},
	{"oauth-client-secret", "--oauth-client-secret", config.HTTP},
	{"oauth-scope", "--oauth-scope", config.HTTP},
}

// upstreams names the Upstreams of each Transport in errors.
var upstreams = map[config.Transport]string{config.Stdio: "a stdio upstream", config.HTTP: "an HTTP upstream"}

// setFields sets the fields of def, an Upstream from add's arguments, that
// add's flags give. A flag that doesn't apply to def's Transport is an
// error rather than a field serve would ignore, and so are a client secret
// without a client ID and an empty OAuth value, which would write nothing.
func setFields(cmd *ucli.Command, def *config.Definition) error {
	transport := def.Transport()
	for _, f := range transportFlags {
		if f.transport != transport && cmd.IsSet(f.name) {
			return fmt.Errorf("%s applies only to %s", f.usage, upstreams[f.transport])
		}
	}
	id, secret, scopes := cmd.String("oauth-client-id"), cmd.String("oauth-client-secret"), cmd.StringSlice("oauth-scope")
	if cmd.IsSet("oauth-client-secret") && id == "" {
		return errors.New("--oauth-client-secret needs --oauth-client-id")
	}
	for _, f := range []struct {
		name  string
		empty bool
	}{
		{"oauth-client-id", id == ""},
		{"oauth-client-secret", secret == ""},
		{"oauth-scope", slices.Contains(scopes, "")},
	} {
		if f.empty && cmd.IsSet(f.name) {
			return fmt.Errorf("--%s must not be empty", f.name)
		}
	}
	var err error
	if def.Env, err = envVars(cmd.StringSlice("env")); err != nil {
		return err
	}
	if def.Headers, err = headers(cmd.StringSlice("header")); err != nil {
		return err
	}
	if id != "" || len(scopes) > 0 {
		def.OAuth = &config.OAuth{ClientID: id, ClientSecret: secret, Scopes: scopes}
	}
	def.Disabled = cmd.Bool("disabled")
	return nil
}

// envVars parses -e values, each KEY=VALUE.
func envVars(values []string) (map[string]string, error) {
	return pairs(values, "KEY=VALUE", func(v string) (string, string, bool) {
		key, value, ok := strings.Cut(v, "=")
		return key, value, ok && key != ""
	})
}

// headers parses -H values, each in curl's form: the name, a colon, and
// the value, with the spaces after the colon dropped.
func headers(values []string) (map[string]string, error) {
	return pairs(values, "Name: value", func(v string) (string, string, bool) {
		name, value, ok := strings.Cut(v, ":")
		return name, strings.TrimLeft(value, " \t"), ok && name != "" && !strings.ContainsAny(name, " \t")
	})
}

// pairs splits each of values into a key and its value with split, the
// last value of a key winning. A value split rejects is not in the form
// want.
func pairs(values []string, want string, split func(string) (string, string, bool)) (map[string]string, error) {
	m := make(map[string]string, len(values))
	for _, v := range values {
		key, value, ok := split(v)
		if !ok {
			return nil, fmt.Errorf("%q is not %s", v, want)
		}
		m[key] = value
	}
	return m, nil
}

// isHTTPURL reports whether s is an http(s) URL, taking the scheme in any
// case, as URLs do.
func isHTTPURL(s string) bool {
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}
