package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/marvell/sprut/internal/config"
)

func TestDocumentAdd(t *testing.T) {
	t.Parallel()
	const before = `{"$schema": "https://example.com/schema.json",
	 "mcpServers": {
		"zeta": {"command": "z", "x-note": {"keep": [1, 2]}, "env": {"K": "${K}"}},
		"alpha": {"url": "https://${HOST}/mcp", "disabled": true}
	 },
	 "extra": [true, null]}`
	tests := []struct {
		name    string
		config  string
		entry   string
		def     config.Definition
		replace bool
		want    string
	}{
		{
			name:   "stdio entry goes last, everything else kept",
			config: before,
			entry:  "memory",
			def:    config.Definition{Command: "npx", Args: []string{"-y", "${PKG}"}},
			want: `{
  "$schema": "https://example.com/schema.json",
  "mcpServers": {
    "zeta": {
      "command": "z",
      "x-note": {
        "keep": [
          1,
          2
        ]
      },
      "env": {
        "K": "${K}"
      }
    },
    "alpha": {
      "url": "https://${HOST}/mcp",
      "disabled": true
    },
    "memory": {
      "command": "npx",
      "args": [
        "-y",
        "${PKG}"
      ]
    }
  },
  "extra": [
    true,
    null
  ]
}
`,
		},
		{
			name:    "replacing keeps the entry's place",
			config:  before,
			entry:   "zeta",
			def:     config.Definition{URL: "https://zeta.example/mcp"},
			replace: true,
			want: `{
  "$schema": "https://example.com/schema.json",
  "mcpServers": {
    "zeta": {
      "url": "https://zeta.example/mcp"
    },
    "alpha": {
      "url": "https://${HOST}/mcp",
      "disabled": true
    }
  },
  "extra": [
    true,
    null
  ]
}
`,
		},
		{
			name:   "replacing a name not there adds it",
			config: `{"mcpServers": {}}`,
			entry:  "a",
			def:    config.Definition{Command: "a"},
			want: `{
  "mcpServers": {
    "a": {
      "command": "a"
    }
  }
}
`,
			replace: true,
		},
		{
			name:   "empty Config gets mcpServers",
			config: `{"other": 1}`,
			entry:  "a",
			def:    config.Definition{Command: "a"},
			want: `{
  "other": 1,
  "mcpServers": {
    "a": {
      "command": "a"
    }
  }
}
`,
		},
		{
			name:   "null mcpServers becomes an object",
			config: `{"mcpServers": null}`,
			entry:  "a",
			def:    config.Definition{URL: "http://a.example"},
			want: `{
  "mcpServers": {
    "a": {
      "url": "http://a.example"
    }
  }
}
`,
		},
		{
			name:   "characters are written as they are",
			config: `{"mcpServers": {}}`,
			entry:  "a",
			def:    config.Definition{Command: "a", Args: []string{"<&>", "é"}},
			want: `{
  "mcpServers": {
    "a": {
      "command": "a",
      "args": [
        "<&>",
        "é"
      ]
    }
  }
}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc, err := config.ParseDocument([]byte(tt.config))
			if err != nil {
				t.Fatal(err)
			}
			if err := doc.Add(tt.entry, tt.def, tt.replace); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, string(doc.Bytes())); diff != "" {
				t.Errorf("Bytes() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDocumentAddExisting(t *testing.T) {
	t.Parallel()
	const before = `{"mcpServers": {"a": {"command": "a"}}}`
	doc, err := config.ParseDocument([]byte(before))
	if err != nil {
		t.Fatal(err)
	}
	err = doc.Add("a", config.Definition{Command: "b"}, false)
	if !errors.Is(err, config.ErrExists) {
		t.Fatalf("Add of an existing name: %v, want ErrExists", err)
	}
	want := "{\n  \"mcpServers\": {\n    \"a\": {\n      \"command\": \"a\"\n    }\n  }\n}\n"
	if got := string(doc.Bytes()); got != want {
		t.Errorf("a failed Add changed the Config to %s", got)
	}
}

// A Config whose own keys repeat or differ in case from mcpServers is
// rewritten so that it still parses the way it did.
func TestDocumentEditsTheServersParseReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, config string
		want         []string
	}{
		{"repeated mcpServers", `{"mcpServers": {"old": {"command": "o"}}, "mcpServers": {"a": {"command": "a"}}}`, []string{"a", "new"}},
		{"mcpServers in another case", `{"MCPServers": {"a": {"command": "a"}}}`, []string{"a", "new"}},
		{"repeated name", `{"mcpServers": {"a": {"command": "1"}, "b": {"command": "b"}, "a": {"command": "2"}}}`, []string{"a", "b", "new"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc, err := config.ParseDocument([]byte(tt.config))
			if err != nil {
				t.Fatal(err)
			}
			if err := doc.Add("new", config.Definition{Command: "n"}, false); err != nil {
				t.Fatal(err)
			}
			entries, _, err := config.ParseEntries(doc.Bytes(), lookup(nil))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range entries {
				names = append(names, e.Name)
			}
			if diff := cmp.Diff(tt.want, names); diff != "" {
				t.Errorf("names after Add (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseDocumentRefusesNonObject(t *testing.T) {
	t.Parallel()
	for _, data := range []string{``, `[]`, `"x"`, `{"mcpServers": []}`, `{"a": 1`} {
		if _, err := config.ParseDocument([]byte(data)); err == nil {
			t.Errorf("ParseDocument(%q) succeeded", data)
		}
	}
}

func TestWriteFile(t *testing.T) {
	t.Parallel()

	t.Run("creates the file and its directories private", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "a", "b", "config.json")
		if err := config.WriteFile(path, []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
		assertMode(t, path, 0o600)
		assertMode(t, filepath.Join(dir, "a", "b"), 0o700|os.ModeDir)
		assertMode(t, filepath.Join(dir, "a"), 0o700|os.ModeDir)
		assertContent(t, path, "{}\n")
	})

	t.Run("replaces a file, keeping its mode and leaving no temp file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := config.WriteFile(path, []byte("new")); err != nil {
			t.Fatal(err)
		}
		assertContent(t, path, "new")
		assertMode(t, path, 0o640)
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Errorf("directory holds %v, want only config.json", files)
		}
	})

	t.Run("writes through a symlink to its target", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		target := filepath.Join(dir, "dotfiles", "config.json")
		if err := os.Mkdir(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "config.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := config.WriteFile(link, []byte("new")); err != nil {
			t.Fatal(err)
		}
		assertContent(t, target, "new")
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the symlink was replaced: %v, %v", fi, err)
		}
	})

	t.Run("leaves an existing directory's mode alone", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "shared")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := config.WriteFile(filepath.Join(dir, "config.json"), []byte("{}")); err != nil {
			t.Fatal(err)
		}
		assertMode(t, dir, 0o755|os.ModeDir)
	})
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode(); got != want {
		t.Errorf("%s has mode %v, want %v", path, got, want)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("%s holds %q, want %q", path, got, want)
	}
}
