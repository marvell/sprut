package config_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/marvell/sprut/internal/config"
)

// lookup returns an environment lookup over vars.
func lookup(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		config   string
		env      map[string]string
		want     []config.Upstream
		warnings []config.Warning
	}{
		{
			name: "stdio entry inferred from command",
			config: `{"mcpServers": {"fs": {
				"command": "npx", "args": ["-y", "server-fs"], "env": {"DEBUG": "1"}
			}}}`,
			want: []config.Upstream{{
				Name: "fs", Transport: config.Stdio,
				Command: "npx", Args: []string{"-y", "server-fs"}, Env: map[string]string{"DEBUG": "1"},
			}},
		},
		{
			name: "http entry inferred from url",
			config: `{"mcpServers": {"remote": {
				"url": "https://example.com/mcp", "headers": {"X-Team": "core"}
			}}}`,
			want: []config.Upstream{{
				Name: "remote", Transport: config.HTTP,
				URL: "https://example.com/mcp", Headers: map[string]string{"X-Team": "core"},
			}},
		},
		{
			name: "upstreams and their warnings in Config order",
			config: `{"mcpServers": {
				"zeta": {"command": "z", "x-zeta": 1},
				"alpha": {"command": "a", "x-alpha": 1},
				"mid": {"command": "m", "x-mid": 1}
			}}`,
			want: []config.Upstream{
				{Name: "zeta", Transport: config.Stdio, Command: "z"},
				{Name: "alpha", Transport: config.Stdio, Command: "a"},
				{Name: "mid", Transport: config.Stdio, Command: "m"},
			},
			warnings: []config.Warning{
				{Upstream: "zeta", Message: `unknown field "x-zeta" ignored`},
				{Upstream: "alpha", Message: `unknown field "x-alpha" ignored`},
				{Upstream: "mid", Message: `unknown field "x-mid" ignored`},
			},
		},
		{
			name: "explicit type matching the fields",
			config: `{"mcpServers": {
				"local": {"type": "stdio", "command": "uvx"},
				"remote": {"type": "http", "url": "https://example.com/mcp"}
			}}`,
			want: []config.Upstream{
				{Name: "local", Transport: config.Stdio, Command: "uvx"},
				{Name: "remote", Transport: config.HTTP, URL: "https://example.com/mcp"},
			},
		},
		{
			name:   "empty mcpServers",
			config: `{"mcpServers": {}}`,
		},
		{
			name:   "no mcpServers",
			config: `{}`,
		},
		{
			name: "sse is skipped with a warning",
			config: `{"mcpServers": {
				"legacy": {"type": "sse", "url": "https://example.com/sse"},
				"local": {"command": "uvx"}
			}}`,
			want: []config.Upstream{{Name: "local", Transport: config.Stdio, Command: "uvx"}},
			warnings: []config.Warning{{
				Upstream: "legacy", Message: "SSE transport is not supported; upstream skipped",
			}},
		},
		{
			name: "disabled is not started",
			config: `{"mcpServers": {
				"off": {"command": "npx", "disabled": true},
				"on": {"command": "uvx", "disabled": false}
			}}`,
			want: []config.Upstream{{Name: "on", Transport: config.Stdio, Command: "uvx"}},
		},
		{
			name: "unknown fields are ignored with a warning each",
			config: `{"mcpServers": {"fs": {
				"command": "npx", "cwd": "/tmp", "autoApprove": ["read"]
			}}}`,
			want: []config.Upstream{{Name: "fs", Transport: config.Stdio, Command: "npx"}},
			warnings: []config.Warning{
				{Upstream: "fs", Message: `unknown field "autoApprove" ignored`},
				{Upstream: "fs", Message: `unknown field "cwd" ignored`},
			},
		},
		{
			name: "fields of the other transport are ignored with a warning",
			config: `{"mcpServers": {
				"local": {"command": "npx", "headers": {"A": "b"}},
				"remote": {"url": "https://example.com/mcp", "args": ["x"], "env": {"A": "b"}}
			}}`,
			want: []config.Upstream{
				{Name: "local", Transport: config.Stdio, Command: "npx"},
				{Name: "remote", Transport: config.HTTP, URL: "https://example.com/mcp"},
			},
			warnings: []config.Warning{
				{Upstream: "local", Message: `field "headers" does not apply to stdio upstreams; ignored`},
				{Upstream: "remote", Message: `field "args" does not apply to http upstreams; ignored`},
				{Upstream: "remote", Message: `field "env" does not apply to http upstreams; ignored`},
			},
		},
		{
			name: "${VAR} in env, args, url and headers",
			config: `{"mcpServers": {
				"local": {
					"command": "npx",
					"args": ["--root", "${HOME}/src", "${A}${B}"],
					"env": {"TOKEN": "${TOKEN}", "MIXED": "x-${A}-y"}
				},
				"remote": {
					"url": "https://${HOST}/mcp",
					"headers": {"Authorization": "Bearer ${TOKEN}"}
				}
			}}`,
			env: map[string]string{"HOME": "/home/me", "A": "a", "B": "b", "TOKEN": "s3cret", "HOST": "example.com"},
			want: []config.Upstream{
				{
					Name: "local", Transport: config.Stdio, Command: "npx",
					Args: []string{"--root", "/home/me/src", "ab"},
					Env:  map[string]string{"TOKEN": "s3cret", "MIXED": "x-a-y"},
				},
				{
					Name: "remote", Transport: config.HTTP, URL: "https://example.com/mcp",
					Headers: map[string]string{"Authorization": "Bearer s3cret"},
				},
			},
		},
		{
			name:   "a variable set to empty is used as empty",
			config: `{"mcpServers": {"fs": {"command": "npx", "env": {"FLAG": "${EMPTY}"}}}}`,
			env:    map[string]string{"EMPTY": ""},
			want: []config.Upstream{{
				Name: "fs", Transport: config.Stdio, Command: "npx", Env: map[string]string{"FLAG": ""},
			}},
		},
		{
			name:   "only ${VAR} is interpolated",
			config: `{"mcpServers": {"fs": {"command": "npx", "args": ["$HOME", "${not a var}", "${", "$${HOME}"]}}}`,
			env:    map[string]string{"HOME": "/home/me"},
			want: []config.Upstream{{
				Name: "fs", Transport: config.Stdio, Command: "npx",
				Args: []string{"$HOME", "${not a var}", "${", "$/home/me"},
			}},
		},
		{
			name: "unset variable skips only that upstream, naming every missing variable",
			config: `{"mcpServers": {
				"github": {"command": "npx", "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}, "args": ["${ORG}", "${ORG}"]},
				"linear": {"url": "https://mcp.linear.app/mcp", "headers": {"Authorization": "Bearer ${LINEAR_KEY}"}},
				"local": {"command": "uvx", "args": ["${HOME}"]}
			}}`,
			env:  map[string]string{"HOME": "/home/me"},
			want: []config.Upstream{{Name: "local", Transport: config.Stdio, Command: "uvx", Args: []string{"/home/me"}}},
			warnings: []config.Warning{
				{Upstream: "github", Message: "unset variables GITHUB_TOKEN, ORG; upstream skipped"},
				{Upstream: "linear", Message: "unset variable LINEAR_KEY; upstream skipped"},
			},
		},
		{
			name: "url empty after interpolation skips only that Upstream",
			config: `{"mcpServers": {
				"remote": {"url": "${ENDPOINT}"},
				"local": {"command": "uvx"}
			}}`,
			env:      map[string]string{"ENDPOINT": ""},
			want:     []config.Upstream{{Name: "local", Transport: config.Stdio, Command: "uvx"}},
			warnings: []config.Warning{{Upstream: "remote", Message: `"url" is empty after interpolation; upstream skipped`}},
		},
		{
			name: "oauth on an HTTP Upstream",
			config: `{"mcpServers": {
				"full": {"url": "https://example.com/mcp", "oauth": {
					"clientId": "sprut", "clientSecret": "s3cret", "scopes": ["read", "write"]
				}},
				"empty": {"url": "https://example.com/mcp", "oauth": {"scopes": []}}
			}}`,
			want: []config.Upstream{
				{
					Name: "full", Transport: config.HTTP, URL: "https://example.com/mcp",
					OAuth: &config.OAuth{ClientID: "sprut", ClientSecret: "s3cret", Scopes: []string{"read", "write"}},
				},
				{
					Name: "empty", Transport: config.HTTP, URL: "https://example.com/mcp",
					OAuth: &config.OAuth{Scopes: []string{}},
				},
			},
		},
		{
			name: "${VAR} in each oauth field",
			config: `{"mcpServers": {"remote": {"url": "https://example.com/mcp", "oauth": {
				"clientId": "${ID}", "clientSecret": "${SECRET}", "scopes": ["${SCOPE}", "x-${SCOPE}"]
			}}}}`,
			env: map[string]string{"ID": "sprut", "SECRET": "s3cret", "SCOPE": "read"},
			want: []config.Upstream{{
				Name: "remote", Transport: config.HTTP, URL: "https://example.com/mcp",
				OAuth: &config.OAuth{ClientID: "sprut", ClientSecret: "s3cret", Scopes: []string{"read", "x-read"}},
			}},
		},
		{
			name: "unset variable in oauth skips only that Upstream",
			config: `{"mcpServers": {
				"id": {"url": "https://example.com/mcp", "oauth": {"clientId": "${ID}"}},
				"secret": {"url": "https://example.com/mcp", "oauth": {"clientId": "sprut", "clientSecret": "${SECRET}"}},
				"scopes": {"url": "https://example.com/mcp", "oauth": {"scopes": ["${SCOPE}"]}},
				"local": {"command": "uvx"}
			}}`,
			want: []config.Upstream{{Name: "local", Transport: config.Stdio, Command: "uvx"}},
			warnings: []config.Warning{
				{Upstream: "id", Message: "unset variable ID; upstream skipped"},
				{Upstream: "secret", Message: "unset variable SECRET; upstream skipped"},
				{Upstream: "scopes", Message: "unset variable SCOPE; upstream skipped"},
			},
		},
		{
			name:     "oauth on a stdio Upstream is ignored with a warning",
			config:   `{"mcpServers": {"local": {"command": "npx", "oauth": {"clientId": "${UNSET}", "x": 1}}}}`,
			want:     []config.Upstream{{Name: "local", Transport: config.Stdio, Command: "npx"}},
			warnings: []config.Warning{{Upstream: "local", Message: `field "oauth" does not apply to stdio upstreams; ignored`}},
		},
		{
			name: "unknown keys in oauth are ignored with a warning each",
			config: `{"mcpServers": {"remote": {"url": "https://example.com/mcp", "oauth": {
				"clientId": "sprut", "redirectUri": "x", "audience": "y"
			}}}}`,
			want: []config.Upstream{{
				Name: "remote", Transport: config.HTTP, URL: "https://example.com/mcp",
				OAuth: &config.OAuth{ClientID: "sprut"},
			}},
			warnings: []config.Warning{
				{Upstream: "remote", Message: `unknown field "oauth.audience" ignored`},
				{Upstream: "remote", Message: `unknown field "oauth.redirectUri" ignored`},
			},
		},
		{
			name:   "oauth.clientSecret without oauth.clientId is ignored with a warning",
			config: `{"mcpServers": {"remote": {"url": "https://example.com/mcp", "oauth": {"clientSecret": "${UNSET}"}}}}`,
			want: []config.Upstream{{
				Name: "remote", Transport: config.HTTP, URL: "https://example.com/mcp", OAuth: &config.OAuth{},
			}},
			warnings: []config.Warning{{Upstream: "remote", Message: `field "oauth.clientSecret" does not apply without "oauth.clientId"; ignored`}},
		},
		{
			name:   "unset variable in a disabled Upstream is not reported",
			config: `{"mcpServers": {"fs": {"command": "npx", "args": ["${NOPE}"], "disabled": true}}}`,
		},
		{
			name:   "null in a disabled Upstream's args is not checked",
			config: `{"mcpServers": {"fs": {"command": "npx", "args": [null], "disabled": true}}}`,
		},
		{
			name:   "repeated mcpServers with the same names: the last one wins",
			config: `{"mcpServers": {"a": {"command": "x"}}, "mcpServers": {"a": {"command": "y"}}}`,
			want:   []config.Upstream{{Name: "a", Transport: config.Stdio, Command: "y"}},
		},
		{
			name: "repeated mcpServers with different names: the earlier ones are dropped",
			config: `{"mcpServers": {"a": {"command": "x"}, "b": {"command": "x"}},
				"mcpServers": {"c": {"command": "y"}, "a": {"command": "y"}}}`,
			want: []config.Upstream{
				{Name: "c", Transport: config.Stdio, Command: "y"},
				{Name: "a", Transport: config.Stdio, Command: "y"},
			},
		},
		{
			name:   "repeated mcpServers, the last one empty",
			config: `{"mcpServers": {"a": {"command": "x"}}, "mcpServers": {}}`,
		},
		{
			name:   "repeated mcpServers, the last one null",
			config: `{"mcpServers": {"a": {"command": "x"}}, "mcpServers": null}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, warnings, err := config.Parse([]byte(tt.config), lookup(tt.env))
			if err != nil {
				t.Fatalf("Parse: unexpected error: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Upstreams (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.warnings, warnings); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
		// wantErr are substrings the error must contain, so that the user
		// can find the mistake.
		wantErr []string
	}{
		{
			name:    "malformed JSON",
			config:  "{\n  \"mcpServers\": {\n    \"fs\": {\"command\": \"npx\",}\n  }\n}",
			wantErr: []string{"line 3, column 29:", "invalid character"},
		},
		{
			name:    "mcpServers is not an object",
			config:  `{"mcpServers": []}`,
			wantErr: []string{"mcpServers"},
		},
		{
			name:    "entry is not an object",
			config:  `{"mcpServers": {"fs": "npx"}}`,
			wantErr: []string{`"fs"`},
		},
		{
			name:    "uppercase name",
			config:  `{"mcpServers": {"FS": {"command": "npx"}}}`,
			wantErr: []string{`"FS"`, "^[a-z0-9-]+$"},
		},
		{
			name:    "underscore in name",
			config:  `{"mcpServers": {"my_fs": {"command": "npx"}}}`,
			wantErr: []string{`"my_fs"`, "^[a-z0-9-]+$"},
		},
		{
			name:    "empty name",
			config:  `{"mcpServers": {"": {"command": "npx"}}}`,
			wantErr: []string{`""`, "^[a-z0-9-]+$"},
		},
		{
			name:    "invalid name even when disabled",
			config:  `{"mcpServers": {"My Server": {"command": "npx", "disabled": true}}}`,
			wantErr: []string{`"My Server"`},
		},
		{
			name:    "both command and url",
			config:  `{"mcpServers": {"fs": {"command": "npx", "url": "https://example.com/mcp"}}}`,
			wantErr: []string{`"fs"`, "command", "url"},
		},
		{
			name:    "neither command nor url",
			config:  `{"mcpServers": {"fs": {"args": ["x"]}}}`,
			wantErr: []string{`"fs"`, "command", "url"},
		},
		{
			name:    "type stdio with url",
			config:  `{"mcpServers": {"fs": {"type": "stdio", "url": "https://example.com/mcp"}}}`,
			wantErr: []string{`"fs"`, "stdio", "command"},
		},
		{
			name:    "type http with command",
			config:  `{"mcpServers": {"fs": {"type": "http", "command": "npx"}}}`,
			wantErr: []string{`"fs"`, "http", "url"},
		},
		{
			name:    "unknown type",
			config:  `{"mcpServers": {"fs": {"type": "websocket", "command": "npx"}}}`,
			wantErr: []string{`"fs"`, "websocket"},
		},
		{
			name:    "field of the wrong JSON type",
			config:  `{"mcpServers": {"fs": {"command": "npx", "args": "-y"}}}`,
			wantErr: []string{`"fs"`, "args"},
		},
		{
			name:    "oauth not an object",
			config:  `{"mcpServers": {"r": {"url": "https://example.com/mcp", "oauth": ["x"]}}}`,
			wantErr: []string{`"r"`, `"oauth" must be an object`},
		},
		{
			name:    "oauth scopes not an array of strings",
			config:  `{"mcpServers": {"r": {"url": "https://example.com/mcp", "oauth": {"scopes": "read"}}}}`,
			wantErr: []string{`"r"`, `oauth: "scopes" must be an array of strings`},
		},
		{
			name:    "null oauth",
			config:  `{"mcpServers": {"r": {"url": "https://example.com/mcp", "oauth": null}}}`,
			wantErr: []string{`"r"`, `"oauth" must be an object`},
		},
		{
			name:    "null oauth clientId",
			config:  `{"mcpServers": {"r": {"url": "https://example.com/mcp", "oauth": {"clientId": null}}}}`,
			wantErr: []string{`"r"`, `"oauth" must be an object`},
		},
		{
			name:    "null oauth scopes element",
			config:  `{"mcpServers": {"r": {"url": "https://example.com/mcp", "oauth": {"scopes": [null]}}}}`,
			wantErr: []string{`"r"`, `oauth: "scopes" must be an array of strings`},
		},
		{
			name:    "null disabled",
			config:  `{"mcpServers": {"fs": {"command": "npx", "disabled": null}}}`,
			wantErr: []string{`"fs"`, `"disabled" must be a boolean`},
		},
		{
			name:    "null type",
			config:  `{"mcpServers": {"fs": {"type": null, "command": "npx"}}}`,
			wantErr: []string{`"fs"`, `"type" must be a string`},
		},
		{
			name:    "null command",
			config:  `{"mcpServers": {"fs": {"command": null, "url": "https://example.com/mcp"}}}`,
			wantErr: []string{`"fs"`, `"command" must be a string`},
		},
		{
			name:    "null url",
			config:  `{"mcpServers": {"fs": {"command": "npx", "url": null}}}`,
			wantErr: []string{`"fs"`, `"url" must be a string`},
		},
		{
			name:    "null args element",
			config:  `{"mcpServers": {"fs": {"command": "npx", "args": ["-y", null]}}}`,
			wantErr: []string{`"fs"`, `"args" must be an array of strings`},
		},
		{
			name:    "null env value",
			config:  `{"mcpServers": {"fs": {"command": "npx", "env": {"TOKEN": null}}}}`,
			wantErr: []string{`"fs"`, `"env" must be an object of strings`},
		},
		{
			name:    "null headers value",
			config:  `{"mcpServers": {"remote": {"url": "https://example.com/mcp", "headers": {"Authorization": null}}}}`,
			wantErr: []string{`"remote"`, `"headers" must be an object of strings`},
		},
		{
			name:    "null env",
			config:  `{"mcpServers": {"fs": {"command": "npx", "env": null}}}`,
			wantErr: []string{`"fs"`, `"env" must be an object of strings`},
		},
		{
			name:    "null args",
			config:  `{"mcpServers": {"fs": {"command": "npx", "args": null}}}`,
			wantErr: []string{`"fs"`, `"args" must be an array of strings`},
		},
		{
			name:    "null headers",
			config:  `{"mcpServers": {"remote": {"url": "https://example.com/mcp", "headers": null}}}`,
			wantErr: []string{`"remote"`, `"headers" must be an object of strings`},
		},
		{
			name:    "every invalid entry is reported",
			config:  `{"mcpServers": {"A": {"command": "npx"}, "b": {}}}`,
			wantErr: []string{`"A"`, `"b"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := config.Parse([]byte(tt.config), lookup(nil))
			if err == nil {
				t.Fatal("Parse: no error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestPath(t *testing.T) {
	t.Parallel()
	all := map[string]string{
		"SPRUT_CONFIG":    "/env/sprut.json",
		"XDG_CONFIG_HOME": "/xdg",
		"HOME":            "/home/me",
	}
	without := func(keys ...string) map[string]string {
		env := maps.Clone(all)
		for _, k := range keys {
			delete(env, k)
		}
		return env
	}
	tests := []struct {
		name string
		flag string
		env  map[string]string
		want string
		// whether the path was named rather than the default location
		wantNamed bool
	}{
		{name: "-c wins", flag: "/flag.json", env: all, want: "/flag.json", wantNamed: true},
		{name: "then SPRUT_CONFIG", env: all, want: "/env/sprut.json", wantNamed: true},
		{name: "then XDG_CONFIG_HOME", env: without("SPRUT_CONFIG"), want: "/xdg/sprut/config.json"},
		{name: "then ~/.config", env: without("SPRUT_CONFIG", "XDG_CONFIG_HOME"), want: "/home/me/.config/sprut/config.json"},
		{
			name: "empty variables count as unset",
			env:  map[string]string{"SPRUT_CONFIG": "", "XDG_CONFIG_HOME": "", "HOME": "/home/me"},
			want: "/home/me/.config/sprut/config.json",
		},
		{
			name: "relative XDG_CONFIG_HOME is ignored",
			env:  map[string]string{"XDG_CONFIG_HOME": "xdg", "HOME": "/home/me"},
			want: "/home/me/.config/sprut/config.json",
		},
		{name: "relative -c is kept as given", flag: "my.json", env: all, want: "my.json", wantNamed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, named, err := config.Path(tt.flag, lookup(tt.env))
			if err != nil {
				t.Fatalf("Path: unexpected error: %v", err)
			}
			if got != tt.want || named != tt.wantNamed {
				t.Errorf("Path = %q, named %v; want %q, named %v", got, named, tt.want, tt.wantNamed)
			}
		})
	}
}

func TestPathWithNowhereToLook(t *testing.T) {
	t.Parallel()
	_, _, err := config.Path("", lookup(nil))
	if err == nil || !strings.Contains(err.Error(), "HOME") {
		t.Errorf("Path error = %v, want one mentioning HOME", err)
	}
}
