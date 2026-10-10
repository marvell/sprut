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

// An entry that repeats "disabled" is switched the way the parser reads it:
// by its last "disabled".
func TestDocumentSetDisabledRepeatedField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, entry string
		disabled    bool
		wantChanged bool
	}{
		{"enable, last true", `{"command": "a", "disabled": false, "disabled": true}`, false, true},
		{"enable, last false", `{"command": "a", "disabled": true, "disabled": false}`, false, false},
		{"disable, last false", `{"command": "a", "disabled": true, "disabled": false}`, true, true},
		{"disable, last true", `{"command": "a", "disabled": false, "disabled": true}`, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc, err := config.ParseDocument([]byte(`{"mcpServers": {"a": ` + tt.entry + `}}`))
			if err != nil {
				t.Fatal(err)
			}
			changed, err := doc.SetDisabled("a", tt.disabled)
			if err != nil {
				t.Fatal(err)
			}
			if changed != tt.wantChanged {
				t.Errorf("SetDisabled(%v) changed = %v, want %v", tt.disabled, changed, tt.wantChanged)
			}
			entries, _, err := config.ParseEntries(doc.Bytes(), lookup(nil))
			if err != nil {
				t.Fatal(err)
			}
			if entries[0].Disabled != tt.disabled {
				t.Errorf("after SetDisabled(%v) the parser reads disabled = %v", tt.disabled, entries[0].Disabled)
			}
		})
	}
}

func TestDocumentSetDisabledNotFound(t *testing.T) {
	t.Parallel()
	doc, err := config.ParseDocument([]byte(`{"mcpServers": {"a": {"command": "a"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doc.SetDisabled("b", true); !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("SetDisabled of a missing name: %v, want ErrNotFound", err)
	}
}

func TestDefinitionTransport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		def  config.Definition
		want config.Transport
	}{
		{config.Definition{Command: "npx"}, config.Stdio},
		{config.Definition{URL: "https://example.com/mcp"}, config.HTTP},
		{config.Definition{Command: "npx", URL: "https://example.com/mcp"}, ""},
		{config.Definition{}, ""},
	}
	for _, tt := range tests {
		if got := tt.def.Transport(); got != tt.want {
			t.Errorf("%+v.Transport() = %q, want %q", tt.def, got, tt.want)
		}
	}
}

func TestDocumentEntries(t *testing.T) {
	t.Parallel()
	doc, err := config.ParseDocument([]byte(`{"mcpServers": {"a": {"command": "a", "x": 1}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Add("b", config.Definition{URL: "https://${HOST}/mcp", Disabled: true}, false); err != nil {
		t.Fatal(err)
	}
	entries, warnings, err := doc.Entries(lookup(map[string]string{"HOST": "example.com"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []config.Entry{
		{Name: "a", Transport: config.Stdio, Command: "a", Upstream: &config.Upstream{Name: "a", Transport: config.Stdio, Command: "a"}},
		{
			Name: "b", Transport: config.HTTP, URL: "https://${HOST}/mcp", Disabled: true,
			Upstream: &config.Upstream{Name: "b", Transport: config.HTTP, URL: "https://example.com/mcp"},
		},
	}
	if diff := cmp.Diff(want, entries); diff != "" {
		t.Errorf("Entries after Add (-want +got):\n%s", diff)
	}
	wantWarnings := []config.Warning{{Upstream: "a", Message: `unknown field "x" ignored`}}
	if diff := cmp.Diff(wantWarnings, warnings); diff != "" {
		t.Errorf("Entries warnings (-want +got):\n%s", diff)
	}
}
