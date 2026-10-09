package config_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/marvell/sprut/internal/config"
)

// FuzzParse checks what Parse promises of any input, not only of the
// Configs the other tests think of: it never panics, gives the same answer
// twice, fails with nothing else, and otherwise returns only Upstreams that
// can be started, with every set variable substituted.
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
