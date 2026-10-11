package config_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/marvell/sprut/internal/config"
)

// add is an edit that adds the stdio Upstream name, running name.
func add(name string) func(*config.Document) (bool, error) {
	return func(doc *config.Document) (bool, error) {
		return true, doc.Add(name, config.Definition{Command: name}, false)
	}
}

func TestEditCreatesAMissingConfigAndReturnsItEdited(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "new", "config.json")

	c, err := config.Edit(path, lookup(nil), add("a"))
	if err != nil {
		t.Fatalf("Edit: unexpected error: %v", err)
	}
	want := []config.Upstream{{Name: "a", Transport: config.Stdio, Command: "a", ConfigPath: path}}
	if diff := cmp.Diff(want, c.Upstreams()); diff != "" {
		t.Errorf("Upstreams mismatch (-want +got):\n%s", diff)
	}
	opened, err := config.Open(path, lookup(nil))
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if diff := cmp.Diff(want, opened.Upstreams()); diff != "" {
		t.Errorf("the written Config's Upstreams mismatch (-want +got):\n%s", diff)
	}
}

// readFile is the contents of the file at path.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEditRefusesABrokenConfigWithoutWriting(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	const broken = `{"mcpServers": {"Bad_Name": {"command": "x"}}}`
	writeFile(t, path, broken)

	called := false
	_, err := config.Edit(path, lookup(nil), func(doc *config.Document) (bool, error) {
		called = true
		return add("a")(doc)
	})
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), `upstream "Bad_Name": invalid name`) {
		t.Errorf("Edit error = %v, want one naming %s and the invalid name", err, path)
	}
	if called {
		t.Error("Edit applied the edit to a broken Config")
	}
	if got := readFile(t, path); got != broken {
		t.Errorf("the Config changed to %q", got)
	}
}

func TestEditRejectsAnInvalidResultWithoutWriting(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	const before = `{"mcpServers": {"a": {"command": "a"}}}`
	writeFile(t, path, before)

	_, err := config.Edit(path, lookup(nil), add("Bad_Name"))
	if err == nil || err.Error() != `upstream "Bad_Name": invalid name, must match ^[a-z0-9-]+$` {
		t.Errorf("Edit error = %v, want the invalid name", err)
	}
	if got := readFile(t, path); got != before {
		t.Errorf("the Config changed to %q", got)
	}
}

func TestEditPassesOnTheEditsError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	const before = `{"mcpServers": {"a": {"command": "a"}}}`
	writeFile(t, path, before)

	_, err := config.Edit(path, lookup(nil), add("a"))
	if !errors.Is(err, config.ErrExists) {
		t.Errorf("Edit error = %v, want ErrExists", err)
	}
	if got := readFile(t, path); got != before {
		t.Errorf("the Config changed to %q", got)
	}
}

func TestEditThatChangesNothingLeavesTheFileAsItIs(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	const before = `{"mcpServers": {"a": {"command": "a", "disabled": true}}}`
	writeFile(t, path, before)

	c, err := config.Edit(path, lookup(nil), func(doc *config.Document) (bool, error) {
		return doc.SetDisabled("a", true)
	})
	if err != nil {
		t.Fatalf("Edit: unexpected error: %v", err)
	}
	if e, err := c.Lookup("a"); err != nil || !e.Disabled {
		t.Errorf("Lookup(a) = %+v, %v; want the disabled entry", e, err)
	}
	if got := readFile(t, path); got != before {
		t.Errorf("the Config changed to %q", got)
	}
}

func TestEditReturnsTheEditedConfigWithItsWarnings(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, `{"x": 1, "mcpServers": {"a": {"url": "https://example.com/${MISSING}", "disabled": true}}}`)

	c, err := config.Edit(path, lookup(nil), func(doc *config.Document) (bool, error) {
		return doc.SetDisabled("a", false)
	})
	if err != nil {
		t.Fatalf("Edit: unexpected error: %v", err)
	}
	want := []config.Warning{{Upstream: "a", Message: "unset variable MISSING; upstream skipped"}}
	if diff := cmp.Diff(want, c.Warnings); diff != "" {
		t.Errorf("Warnings mismatch (-want +got):\n%s", diff)
	}
	if e, err := c.Lookup("a"); err != nil || e.Disabled {
		t.Errorf("Lookup(a) = %+v, %v; want the enabled entry", e, err)
	}
	const written = `{
  "x": 1,
  "mcpServers": {
    "a": {
      "url": "https://example.com/${MISSING}"
    }
  }
}
`
	if got := readFile(t, path); got != written {
		t.Errorf("the Config is %q, want %q", got, written)
	}
}

func TestEditWritesASymlinkedConfigAtItsTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "config.json")
	writeFile(t, target, `{}`)
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(filepath.Join("dotfiles", "config.json"), link); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Edit(link, lookup(nil), add("a")); err != nil {
		t.Fatalf("Edit: unexpected error: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the link is no longer a symlink (%v)", err)
	}
	if got := readFile(t, target); !strings.Contains(got, `"a"`) {
		t.Errorf("the target is %q, want it to hold a", got)
	}
}

func TestEditKeepsTheConfigsMode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, `{}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := config.Edit(path, lookup(nil), add("a")); err != nil {
		t.Fatalf("Edit: unexpected error: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("the Config's mode = %v, want 0644", fi.Mode().Perm())
	}
}

func TestEditCreatesANewConfigPrivate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "new", "config.json")

	if _, err := config.Edit(path, lookup(nil), add("a")); err != nil {
		t.Fatalf("Edit: unexpected error: %v", err)
	}
	for p, want := range map[string]fs.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode = %v, want %v", p, fi.Mode().Perm(), want)
		}
	}
}
