// Package statedir locates sprut's state directory, where the Credentials
// and the Audit log live, and makes directories under it private.
package statedir

import (
	"fmt"
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
// already exist. It fails, touching nothing, if dir is not root or under
// it: the directories above root are not sprut's to change.
func MkdirPrivate(root, dir string) error {
	// Rel compares the paths as cleaned, so any spelling of root is found
	// on the way up from dir.
	rel, err := filepath.Rel(root, dir)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("making %s private: not under %s", dir, root)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root = filepath.Clean(root)
	for d := filepath.Join(root, rel); ; d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
		if d == root {
			return nil
		}
	}
}
