package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServeListsUpstreamToolsAsNamespacedTools(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)

	res, err := g.Agent.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	got := map[string]any{}
	for _, tool := range res.Tools {
		got[tool.Name] = toJSON(t, tool)
	}
	want := map[string]any{
		"fake__echo": toJSON(t, map[string]any{
			"name":         "fake__echo",
			"title":        "Echo",
			"description":  "Echoes its text argument back.",
			"inputSchema":  echoInputSchema,
			"outputSchema": echoOutputSchema,
		}),
		"fake__fail": toJSON(t, map[string]any{
			"name":        "fake__fail",
			"description": "Always returns an isError result.",
			"inputSchema": map[string]any{"type": "object"},
		}),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tools/list:\n got %v\nwant %v", got, want)
	}
}

func TestServeRoutesToolCallsToUpstreamAndReturnsResultUnchanged(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)
	ctx := context.Background()

	tests := []struct {
		tool string
		args map[string]any
		want map[string]any
	}{
		{
			tool: "fake__echo",
			args: map[string]any{"text": "hello"},
			want: map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": "echo: hello"}},
				"structuredContent": map[string]any{"echoed": "hello"},
				"_meta":             map[string]any{"fake/trace": "abc"},
			},
		},
		{
			tool: "fake__fail",
			want: map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "it failed"}},
				"isError": true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			res, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: tt.tool, Arguments: tt.args})
			if err != nil {
				t.Fatalf("tools/call: %v", err)
			}
			if got, want := callResult(t, res), toJSON(t, tt.want); !reflect.DeepEqual(got, want) {
				t.Errorf("result:\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestServeIdentifiesAsSprutAndAdvertisesOnlyTools(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)

	init := g.Agent.InitializeResult()
	if init.ServerInfo == nil || init.ServerInfo.Name != "sprut" || init.ServerInfo.Version == "" {
		t.Errorf("serverInfo = %+v, want name sprut with a version", init.ServerInfo)
	}
	if got, want := toJSON(t, init.Capabilities), toJSON(t, map[string]any{"tools": map[string]any{}}); !reflect.DeepEqual(got, want) {
		t.Errorf("capabilities = %v, want %v", got, want)
	}
}

func TestServeShutsDownGatewayAndUpstreamOnStdinEOF(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "upstream.pid")
	g := startFakeGateway(t, envFakePIDFile+"="+pidFile)

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake Upstream did not record its PID: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}

	if code := g.closeAgent(t); code != 0 {
		t.Errorf("exit code = %d, want 0\nstderr:\n%s", code, g.stderr)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("Upstream process %d still exists after Gateway exit (kill -0: %v)", pid, err)
	}
}

func TestServeLogsLogfmtToStderrAndWritesOnlyMCPToStdout(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)
	ctx := context.Background()
	if _, err := g.Agent.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "fake__echo", Arguments: map[string]any{"text": "x"}}); err != nil {
		t.Fatal(err)
	}
	g.closeAgent(t)

	for _, line := range g.stdout.Lines() {
		var msg struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.JSONRPC != "2.0" {
			t.Errorf("stdout line is not a JSON-RPC message: %q", line)
		}
	}

	logfmt := regexp.MustCompile(`^time=\S+ level=(DEBUG|INFO|WARN|ERROR) msg=`)
	ready := false
	for _, line := range g.stderr.Lines() {
		if !logfmt.MatchString(line) {
			t.Errorf("stderr line is not logfmt: %q", line)
		}
		if strings.Contains(line, "level=INFO") && strings.Contains(line, "upstream=fake") &&
			strings.Contains(line, "tools=2") && strings.Contains(line, "protocol=") {
			ready = true
		}
	}
	if !ready {
		t.Errorf("no INFO line reporting Upstream fake ready with tools=2 and protocol; stderr:\n%s", g.stderr)
	}
}

func TestServeInterpolatesVariablesIntoUpstreamEnvironmentOverInheritedOne(t *testing.T) {
	t.Parallel()
	envFile := filepath.Join(t.TempDir(), "upstream.env")
	entry := fakeUpstreamEntry(t)
	entry["env"] = map[string]string{
		"SPRUT_TEST_SECRET":   "token-${SECRET}",
		"SPRUT_TEST_OVERRIDE": "from-config",
	}
	g := startGateway(t, writeConfig(t, map[string]any{"fake": entry}), []string{
		envFakeEnvFile + "=" + envFile,
		"SECRET=abc",
		"SPRUT_TEST_OVERRIDE=from-sprut",
		"SPRUT_TEST_INHERITED=yes",
	})
	g.closeAgent(t)

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("fake Upstream did not record its environment: %v", err)
	}
	got := map[string][]string{}
	for line := range strings.Lines(string(data)) {
		k, v, _ := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		got[k] = append(got[k], v)
	}
	for k, want := range map[string]string{
		"SPRUT_TEST_SECRET":    "token-abc",
		"SPRUT_TEST_OVERRIDE":  "from-config",
		"SPRUT_TEST_INHERITED": "yes",
	} {
		if !reflect.DeepEqual(got[k], []string{want}) {
			t.Errorf("Upstream environment %s = %q, want [%q]", k, got[k], want)
		}
	}
}

func TestServeSkipsUpstreamWithUnsetVariableAndServesTheOthers(t *testing.T) {
	t.Parallel()
	broken := fakeUpstreamEntry(t)
	broken["env"] = map[string]string{"TOKEN": "${SPRUT_TEST_UNSET}"}
	g := startGateway(t, writeConfig(t, map[string]any{"fake": fakeUpstreamEntry(t), "broken": broken}), nil)

	res, err := g.Agent.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if want := []string{"fake__echo", "fake__fail"}; !reflect.DeepEqual(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}

	warned := false
	for _, line := range g.stderr.Lines() {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "upstream=broken") &&
			strings.Contains(line, "SPRUT_TEST_UNSET") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no WARN line naming Upstream broken and SPRUT_TEST_UNSET; stderr:\n%s", g.stderr)
	}
}

func TestServeConfigErrorsExit1(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	missing := filepath.Join(dir, "missing.json")
	malformed := write("malformed.json", `{"mcpServers": {`)
	badName := write("bad-name.json", `{"mcpServers": {"My_Server": {"command": "x"}}}`)

	tests := []struct {
		name string
		env  []string
		args []string
		// wantStderr is what stderr must contain to point the user at the mistake.
		wantStderr []string
	}{
		{name: "missing file from -c", args: []string{"serve", "-c", missing}, wantStderr: []string{missing}},
		{name: "missing file from SPRUT_CONFIG", env: []string{"SPRUT_CONFIG=" + missing}, args: []string{"serve"}, wantStderr: []string{missing}},
		{
			name:       "missing default file",
			env:        []string{"HOME=" + dir},
			args:       []string{"serve"},
			wantStderr: []string{filepath.Join(dir, ".config", "sprut", "config.json")},
		},
		{name: "malformed JSON", args: []string{"serve", "-c", malformed}, wantStderr: []string{malformed, "unexpected end of JSON input"}},
		{name: "invalid Upstream name", args: []string{"serve", "-c", badName}, wantStderr: []string{"My_Server"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := runSprut(t, tt.env, tt.args...)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			for _, want := range tt.wantStderr {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr does not contain %q:\n%s", want, stderr)
				}
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}
