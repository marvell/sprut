package cli

import (
	"context"
	"fmt"
	"io"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
)

// switchCommand returns disable, which switches an Upstream off, when
// disabled is set, or else enable, which switches it on.
func switchCommand(disabled bool, env []string, stdout, stderr io.Writer) *ucli.Command {
	name, done, usage := "enable", "enabled", "switch a disabled Upstream back on; refused if its entry is invalid"
	if disabled {
		name, done, usage = "disable", "disabled", "switch an Upstream off without removing it from the Config"
	}
	return &ucli.Command{
		Name:      name,
		Usage:     usage,
		Arguments: []ucli.Argument{&ucli.StringArg{Name: "name", UsageText: "<name>", Required: true}},
		Action: func(_ context.Context, cmd *ucli.Command) error {
			upstream := cmd.StringArg("name")
			var switched bool
			err := editConfig(cmd.String("config"), config.LookupIn(env), stderr, upstream, func(doc *config.Document) (bool, error) {
				var err error
				switched, err = doc.SetDisabled(upstream, disabled)
				return switched, err
			})
			if err != nil {
				return err
			}
			if !switched {
				_, _ = fmt.Fprintf(stdout, "upstream %q is already %s\n", upstream, done)
				return nil
			}
			changed(stdout, stderr, "%s upstream %q", done, upstream)
			return nil
		},
	}
}
