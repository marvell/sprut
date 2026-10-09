package config_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/marvell/sprut/internal/config"
)

// FuzzParse checks what Parse promises of any input, not only of the
// Configs the other tests think of: it never panics, gives the same answer
// twice, fails with nothing else, and otherwise returns only Upstreams that
// can be started, with every set variable substituted. It also checks that
// a Config that parses is written back as a Document that parses the same,
// and still does with an Upstream added.
func FuzzParse(f *testing.F) {
	example, err := os.ReadFile("../../examples/config.json")
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{
		string(example),
		`{"mcpServers": {}}`,
		`{"mcpServers": {"fs": {"command": "npx", "args": ["-y", "${SET}"], "env": {"K": "${SET}"}}}}`,
		`{"mcpServers": {"remote": {"url": "https://${SET}/mcp", "headers": {"A": "${SET}"}, "oauth": {"clientId": "${SET}", "scopes": []}}}}`,
		`{"mcpServers": {"x": {"type": "sse", "url": "https://e.example"}, "y": {"command": "c", "disabled": true}}}`,
		`{"mcpServers": {"a": {"command": "x"}}, "mcpServers": null}`,
		`{"mcpServers": {"u": {"url": "${EMPTY}"}, "v": {"command": "c", "args": ["${UNSET}"]}}}`,
		`{"MCPServers": {"a": {"command": "x"}}, "x": [1, {"y": "<&>"}], "mcpServers": {"b": {"url": "u"}, "b": {"command": "c"}}}`,
		`null`,
		`{"mcpſervers": {"a": {"command": "x"}}}`,
	} {
		f.Add([]byte(seed))
	}
	env := lookup(map[string]string{"SET": "value", "EMPTY": ""})
	validName := regexp.MustCompile(`^[a-z0-9-]+$`)

	f.Fuzz(func(t *testing.T, data []byte) {
		upstreams, warnings, err := config.Parse(data, env)
		again, warningsAgain, errAgain := config.Parse(data, env)
		if diff := cmp.Diff(upstreams, again); diff != "" || !slices.Equal(warnings, warningsAgain) || (err == nil) != (errAgain == nil) {
			t.Fatalf("Parse is not deterministic:\n%s", diff)
		}
		if err != nil {
			if upstreams != nil || warnings != nil {
				t.Fatalf("Parse failed with %v, and returned %v, %v too", err, upstreams, warnings)
			}
			return
		}

		checkRewrite(t, data, env)

		seen := map[string]bool{}
		for _, u := range upstreams {
			if !validName.MatchString(u.Name) || seen[u.Name] {
				t.Errorf("Upstream name %q is invalid or repeated", u.Name)
			}
			seen[u.Name] = true
			switch {
			case u.Transport == config.Stdio && u.Command == "":
				t.Errorf("stdio Upstream %q has no command", u.Name)
			case u.Transport == config.HTTP && u.URL == "":
				t.Errorf("HTTP Upstream %q has no url", u.Name)
			case u.Transport != config.Stdio && u.Transport != config.HTTP:
				t.Errorf("Upstream %q has transport %q", u.Name, u.Transport)
			}
			for _, v := range interpolated(u) {
				if strings.Contains(v, "${SET}") || strings.Contains(v, "${EMPTY}") {
					t.Errorf("Upstream %q keeps a set variable unsubstituted: %q", u.Name, v)
				}
			}
		}
	})
}

// interpolated returns the values of u that ${VAR} references are
// substituted in.
func interpolated(u config.Upstream) []string {
	out := append([]string{u.URL}, u.Args...)
	for _, m := range []map[string]string{u.Env, u.Headers} {
		for _, v := range m {
			out = append(out, v)
		}
	}
	if u.OAuth != nil {
		out = append(out, u.OAuth.ClientID, u.OAuth.ClientSecret)
		out = append(out, u.OAuth.Scopes...)
	}
	return out
}

// checkRewrite checks that the Config data, which parses, is written back
// as a Document that parses the same, and that adding an Upstream to it
// adds just that one after the others.
func checkRewrite(t *testing.T, data []byte, env config.LookupEnv) {
	t.Helper()
	want, wantWarnings, err := config.ParseEntries(data, env)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.ParseDocument(data)
	if err != nil {
		t.Fatalf("ParseDocument failed on a Config that parses: %v", err)
	}
	got, warnings, err := config.ParseEntries(doc.Bytes(), env)
	if err != nil {
		t.Fatalf("the Config written back fails to parse: %v\n%s", err, doc.Bytes())
	}
	if diff := cmp.Diff(want, got); diff != "" || !slices.Equal(warnings, wantWarnings) {
		t.Fatalf("the Config written back parses differently (-want +got):\n%s\n%s", diff, doc.Bytes())
	}

	const name = "fuzz-added"
	if slices.ContainsFunc(want, func(e config.Entry) bool { return e.Name == name }) {
		return
	}
	if err := doc.Add(name, config.Definition{Command: "c"}, false); err != nil {
		t.Fatal(err)
	}
	got, _, err = config.ParseEntries(doc.Bytes(), env)
	if err != nil {
		t.Fatalf("the Config with an Upstream added fails to parse: %v\n%s", err, doc.Bytes())
	}
	if len(got) != len(want)+1 || got[len(want)].Name != name {
		t.Fatalf("adding %q gave entries %v", name, got)
	}
	if diff := cmp.Diff(want, got[:len(want)], cmpopts.EquateEmpty()); diff != "" {
		t.Fatalf("adding an Upstream changed the others (-want +got):\n%s", diff)
	}
}
