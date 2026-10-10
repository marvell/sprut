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

// writeFile writes data at path, making its directory.
func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

const openConfig = `{"mcpServers": {
	"on": {"url": "https://example.com/on"},
	"off": {"url": "https://example.com/off", "disabled": true},
	"unset": {"url": "https://example.com/${MISSING}"},
	"off-broken": {"type": "stdio", "url": "https://example.com/x", "disabled": true}
}}`

func TestOpenNamedConfigGivesEveryResolvedUpstreamItsAbsolutePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeFile(t, path, openConfig)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, path)
	if err != nil {
		t.Fatal(err)
	}

	for name, open := range map[string]func() (*config.Config, error){
		"relative -c": func() (*config.Config, error) { return config.Open(rel, lookup(nil)) },
		"SPRUT_CONFIG": func() (*config.Config, error) {
			return config.Open("", lookup(map[string]string{"SPRUT_CONFIG": path}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := open()
			if err != nil {
				t.Fatalf("Open: unexpected error: %v", err)
			}
			if c.Path != path {
				t.Errorf("Path = %q, want %q", c.Path, path)
			}
			want := []config.Upstream{
				{Name: "on", Transport: config.HTTP, URL: "https://example.com/on", ConfigPath: path},
			}
			if diff := cmp.Diff(want, c.Upstreams()); diff != "" {
				t.Errorf("Upstreams mismatch (-want +got):\n%s", diff)
			}
			u, err := c.Upstream("off")
			if err != nil {
				t.Fatalf("Upstream(off): unexpected error: %v", err)
			}
			if u.ConfigPath != path {
				t.Errorf("the disabled Upstream's ConfigPath = %q, want %q", u.ConfigPath, path)
			}
			wantWarnings := []config.Warning{{Upstream: "unset", Message: "unset variable MISSING; upstream skipped"}}
			if diff := cmp.Diff(wantWarnings, c.Warnings); diff != "" {
				t.Errorf("Warnings mismatch (-want +got):\n%s", diff)
			}
			if len(c.Entries) != 4 {
				t.Errorf("got %d entries, want all 4", len(c.Entries))
			}
		})
	}
}

func TestOpenConfigAtTheDefaultLocationLeavesConfigPathEmpty(t *testing.T) {
	t.Parallel()
	xdg := t.TempDir()
	path := filepath.Join(xdg, "sprut", "config.json")
	writeFile(t, path, openConfig)

	c, err := config.Open("", lookup(map[string]string{"XDG_CONFIG_HOME": xdg}))
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if c.Path != path {
		t.Errorf("Path = %q, want %q", c.Path, path)
	}
	for _, name := range []string{"on", "off"} {
		u, err := c.Upstream(name)
		if err != nil {
			t.Fatalf("Upstream(%s): unexpected error: %v", name, err)
		}
		if u.ConfigPath != "" {
			t.Errorf("Upstream(%s).ConfigPath = %q, want empty", name, u.ConfigPath)
		}
	}
}

func TestOpenMissingConfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing.json")

	c, err := config.Open(path, lookup(nil))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open error = %v, want an fs.ErrNotExist", err)
	}
	if want := "reading config: open " + path + ": "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Open error = %q, want it to start with %q", err, want)
	}
	// An empty Config at that path, which an edit can create.
	if c == nil || c.Path != path || len(c.Entries) != 0 {
		t.Fatalf("Open = %+v, want an empty Config at %s", c, path)
	}
	if _, err := c.Lookup("any"); !errors.Is(err, config.ErrNotFound) {
		t.Errorf("Lookup in the missing Config: error = %v, want ErrNotFound", err)
	}
}

func TestOpenFailures(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, `{"mcpServers": {"bad name": {"command": "x"}}}`)
	malformed := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, malformed, `{"mcpServers": `)

	tests := []struct {
		name string
		flag string
		env  map[string]string
		want string
	}{
		{name: "invalid entry", flag: path, want: "config " + path + `: upstream "bad name": invalid name`},
		{name: "malformed JSON", flag: malformed, want: "config " + malformed + ": "},
		{name: "nowhere to look", want: "cannot locate the config: HOME is not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := config.Open(tt.flag, lookup(tt.env))
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("Open error = %v, want one starting with %q", err, tt.want)
			}
			if c != nil {
				t.Errorf("Open = %+v, want no Config", c)
			}
		})
	}
}

func TestConfigLookupCoversEveryEntry(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, openConfig)
	c, err := config.Open(path, lookup(nil))
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}

	for _, name := range []string{"on", "off", "unset", "off-broken"} {
		e, err := c.Lookup(name)
		if err != nil || e.Name != name {
			t.Errorf("Lookup(%s) = %q, %v; want that entry", name, e.Name, err)
		}
	}
	_, err = c.Lookup("nope")
	if !errors.Is(err, config.ErrNotFound) || err.Error() != `upstream "nope": not in the config` {
		t.Errorf("Lookup(nope) error = %v, want ErrNotFound naming nope", err)
	}

	tests := []struct {
		name    string
		wantURL string
		wantErr string
	}{
		{name: "on", wantURL: "https://example.com/on"},
		{name: "off", wantURL: "https://example.com/off"},
		{name: "unset", wantErr: `upstream "unset": unset variable MISSING`},
		{name: "off-broken", wantErr: `upstream "off-broken": disabled, and type "stdio" needs "command" and no "url"`},
		{name: "nope", wantErr: `upstream "nope": not in the config`},
	}
	for _, tt := range tests {
		u, err := c.Upstream(tt.name)
		switch {
		case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
			t.Errorf("Upstream(%s) error = %v, want %q", tt.name, err, tt.wantErr)
		case tt.wantErr == "" && (err != nil || u.URL != tt.wantURL):
			t.Errorf("Upstream(%s) = %+v, %v; want the one at %s", tt.name, u, err, tt.wantURL)
		}
	}
}

func TestConfigDocumentEditsLeaveTheConfigAsOpened(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, openConfig)
	c, err := config.Open(path, lookup(nil))
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if err := c.Document().Remove("on"); err != nil {
		t.Fatalf("Remove: unexpected error: %v", err)
	}
	entries, _, err := config.ParseEntries(c.Document().Bytes(), lookup(nil))
	if err != nil || len(entries) != 4 || entries[0].Name != "on" {
		t.Errorf("a second Document has %d entries (%v), want all 4, starting with on", len(entries), err)
	}
}
