package cli

import (
	"context"
	"fmt"
	"io"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
)

// switchCommand returns the command name, which switches an Upstream off
// when disabled is set, or on otherwise.
func switchCommand(name, usage string, disabled bool, env []string, stdout, stderr io.Writer) *ucli.Command {
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
				_, _ = fmt.Fprintf(stdout, "upstream %q is already %sd\n", upstream, name)
				return nil
			}
			changed(stdout, stderr, "%sd upstream %q", name, upstream)
			return nil
		},
	}
}
