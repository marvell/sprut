// Package statedir locates sprut's state directory, where the Credentials
// and the Audit log live, and makes directories under it private.
package statedir

import (
	"os"
	"path/filepath"

	"github.com/marvell/sprut/internal/config"
)

// Dir is sprut's state directory: $XDG_STATE_HOME/sprut, or
// ~/.local/state/sprut. It reports false if neither is set.
func Dir(lookupEnv config.LookupEnv) (string, bool) {
	dir, ok := config.XDGDir(lookupEnv, "XDG_STATE_HOME", filepath.Join(".local", "state"))
	if !ok {
		return "", false
	}
	return filepath.Join(dir, "sprut"), true
}

// MkdirPrivate makes dir, which is root or under it, and every directory
// from root down to it readable only by the user, including those that
// already exist.
func MkdirPrivate(root, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for d := dir; ; d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
		if d == root || d == filepath.Dir(d) {
			return nil
		}
	}
}
