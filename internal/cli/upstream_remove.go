package cli

import (
	"context"
	"fmt"
	"io"

	ucli "github.com/urfave/cli/v3"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

func removeCommand(env []string, stdout, stderr io.Writer) *ucli.Command {
	return &ucli.Command{
		Name:      "remove",
		Usage:     "remove an upstream from the config, and delete its credentials",
		Arguments: []ucli.Argument{&ucli.StringArg{Name: "name", UsageText: "<name>", Required: true}},
		Action: func(ctx context.Context, cmd *ucli.Command) error {
			name := cmd.StringArg("name")
			lookupEnv := config.LookupIn(env)
			// Before the Config changes, so that a Store it can't find
			// leaves the Config as it was.
			store, err := credentials.NewStore(lookupEnv)
			if err != nil {
				return err
			}
			err = editConfig(cmd.String("config"), lookupEnv, stderr, name, func(doc *config.Document) (bool, error) {
				return true, doc.Remove(name)
			})
			if err != nil {
				return err
			}
			// Only once the Config is saved, so that a Config that can't be
			// saved keeps an Upstream with its Credentials.
			had, err := store.Delete(ctx, name)
			format := "removed upstream %q"
			if had {
				format += " and deleted its credentials"
			}
			changed(stdout, stderr, format, name)
			if err != nil {
				return fmt.Errorf("upstream %q: %w", name, err)
			}
			return nil
		},
	}
}
