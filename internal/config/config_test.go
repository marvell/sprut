package config_test

import (
	"maps"
	"reflect"
	"strings"
	"testing"

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
			name:   "unset variable in a disabled upstream is not reported",
			config: `{"mcpServers": {"fs": {"command": "npx", "args": ["${NOPE}"], "disabled": true}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, warnings, err := config.Parse([]byte(tt.config), lookup(tt.env))
			if err != nil {
				t.Fatalf("Parse: unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Upstreams:\n got %+v\nwant %+v", got, tt.want)
			}
			if !reflect.DeepEqual(warnings, tt.warnings) {
				t.Errorf("warnings:\n got %+v\nwant %+v", warnings, tt.warnings)
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
	}{
		{name: "-c wins", flag: "/flag.json", env: all, want: "/flag.json"},
		{name: "then SPRUT_CONFIG", env: all, want: "/env/sprut.json"},
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
		{name: "relative -c is kept as given", flag: "my.json", env: all, want: "my.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.Path(tt.flag, lookup(tt.env))
			if err != nil {
				t.Fatalf("Path: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("Path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPathWithNowhereToLook(t *testing.T) {
	t.Parallel()
	_, err := config.Path("", lookup(nil))
	if err == nil || !strings.Contains(err.Error(), "HOME") {
		t.Errorf("Path error = %v, want one mentioning HOME", err)
	}
}
