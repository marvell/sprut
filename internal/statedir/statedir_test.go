package statedir_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marvell/sprut/internal/statedir"
)

// mode returns the permission bits of the directory at path.
func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// mkdir makes the directory at path, and those above it, with mode 0755
// whatever the umask.
func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // the test needs a directory that is not private
		t.Fatal(err)
	}
}

func TestMkdirPrivate(t *testing.T) {
	t.Parallel()
	sep := string(filepath.Separator)
	tests := []struct {
		name string
		// root and dir build the arguments from the state directory.
		root func(state string) string
		dir  func(state string) string
	}{
		{
			name: "directory under the root",
			root: func(state string) string { return state },
			dir:  func(state string) string { return filepath.Join(state, "audit", "agent") },
		},
		{
			name: "the root itself",
			root: func(state string) string { return state },
			dir:  func(state string) string { return state },
		},
		{
			name: "root with a trailing separator",
			root: func(state string) string { return state + sep },
			dir:  func(state string) string { return filepath.Join(state, "audit", "agent") },
		},
		{
			name: "root spelled with a parent directory",
			root: func(state string) string { return state + sep + "audit" + sep + ".." },
			dir:  func(state string) string { return filepath.Join(state, "audit", "agent") },
		},
		{
			name: "directory that is not a cleaned path",
			root: func(state string) string { return state },
			dir:  func(state string) string { return state + sep + "audit" + sep + sep + "agent" + sep },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := filepath.Join(t.TempDir(), "home")
			state := filepath.Join(home, "state")
			// The state directory and one below it exist and are not private.
			mkdir(t, home)
			mkdir(t, state)
			mkdir(t, filepath.Join(state, "audit"))

			root, dir := tt.root(state), tt.dir(state)
			if err := statedir.MkdirPrivate(root, dir); err != nil {
				t.Fatalf("MkdirPrivate(%q, %q): %v", root, dir, err)
			}

			for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
				if got := mode(t, d); got != 0o700 {
					t.Errorf("mode of %s = %o, want 700", d, got)
				}
				if d == state {
					break
				}
			}
			if got := mode(t, home); got != 0o755 {
				t.Errorf("mode of the directory above the root = %o, want 755 as before", got)
			}
		})
	}
}

func TestMkdirPrivateOutsideRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// dir builds the directory from the one that holds the state directory.
		dir func(home string) string
	}{
		{name: "sibling of the root", dir: func(home string) string { return filepath.Join(home, "other", "new") }},
		{name: "parent of the root", dir: func(home string) string { return home }},
		{
			name: "path that leaves the root",
			dir:  func(home string) string { return filepath.Join(home, "state") + "/../other/new" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := filepath.Join(t.TempDir(), "home")
			state := filepath.Join(home, "state")
			other := filepath.Join(home, "other")
			mkdir(t, home)
			mkdir(t, state)
			mkdir(t, other)

			dir := tt.dir(home)
			if err := statedir.MkdirPrivate(state, dir); err == nil {
				t.Fatalf("MkdirPrivate(%q, %q) = nil, want an error", state, dir)
			}

			for _, d := range []string{home, state, other} {
				if got := mode(t, d); got != 0o755 {
					t.Errorf("mode of %s = %o, want 755 as before", d, got)
				}
			}
			if _, err := os.Stat(filepath.Join(other, "new")); !os.IsNotExist(err) {
				t.Errorf("stat of the directory outside the root: %v, want it not to exist", err)
			}
		})
	}
}
