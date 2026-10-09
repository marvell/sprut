package config_test

import (
	"errors"
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
