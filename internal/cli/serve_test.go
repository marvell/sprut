package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

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

	g.closeAgent(t)
	wantUpstreamGone(t, pidFile)
}

func TestServeLogsLogfmtAtInfoToStderrAndWritesOnlyMCPToStdout(t *testing.T) {
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

	// Without -v nothing is logged at DEBUG, not even the tool call.
	logfmt := regexp.MustCompile(`^time=\S+ level=(INFO|WARN|ERROR) msg=`)
	for _, line := range g.stderr.Lines() {
		if !logfmt.MatchString(line) {
			t.Errorf("stderr line is not logfmt at INFO or above: %q", line)
		}
	}
	g.wantLogLine(t, "INFO", "upstream=fake", "tools=2", "protocol=")
}

func TestServeInterpolatesVariablesIntoUpstreamEnvironmentOverInheritedOne(t *testing.T) {
	t.Parallel()
	envFile := filepath.Join(t.TempDir(), "upstream.env")
	entry := fakeUpstreamEntry(t, "SPRUT_TEST_SECRET=token-${SECRET}", "SPRUT_TEST_OVERRIDE=from-config")
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
	for _, line := range lines(string(data)) {
		k, v, _ := strings.Cut(line, "=")
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
	broken := fakeUpstreamEntry(t, "TOKEN=${SPRUT_TEST_UNSET}")
	g := startGateway(t, writeConfig(t, map[string]any{"fake": fakeUpstreamEntry(t), "broken": broken}), nil)

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", "upstream=broken", "SPRUT_TEST_UNSET")
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

func TestServeExcludesUpstreamThatFailsAtStartupAndServesTheOthers(t *testing.T) {
	t.Parallel()
	failing := fakeUpstreamEntry(t, envFakeStartup+"=fail")
	g := startGateway(t, writeConfig(t, map[string]any{"fake": fakeUpstreamEntry(t), "failing": failing}), nil)

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", "upstream=failing", "err=")
	g.wantLogLine(t, "INFO", "ready=1", "failed=1", "tools=2")
	g.closeAgent(t)
}

func TestServeExcludesUpstreamThatHangsPastStartupTimeout(t *testing.T) {
	t.Parallel()
	hanging := fakeUpstreamEntry(t, envFakeStartup+"=hang")
	begin := time.Now()
	g := startGateway(t, writeConfig(t, map[string]any{"fake": fakeUpstreamEntry(t), "hanging": hanging}), nil,
		"--startup-timeout", "300ms")
	// Well under the ~5s grace a hung Upstream gets to exit once stopped:
	// stopping it must not hold up startup.
	if took := time.Since(begin); took > 3*time.Second {
		t.Errorf("startup took %s with a 300ms startup timeout", took)
	}

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", "upstream=hanging", "err=", "300ms")
	g.closeAgent(t)
}

func TestServeStartsUpstreamsConcurrently(t *testing.T) {
	t.Parallel()
	// Each Upstream waits for all three to be running before it answers, so
	// started one after another the first would never become ready.
	rendezvous := envFakeRendezvous + "=3:" + t.TempDir()
	g := startGateway(t, writeConfig(t, map[string]any{
		"one":   fakeUpstreamEntry(t, rendezvous),
		"two":   fakeUpstreamEntry(t, rendezvous),
		"three": fakeUpstreamEntry(t, rendezvous),
	}), nil, "--startup-timeout", "5s")

	g.wantTools(t, "one__echo", "one__fail", "three__echo", "three__fail", "two__echo", "two__fail")
}

func TestServeSkipsToolWhoseNamespacedNameBreaksTheNameRules(t *testing.T) {
	t.Parallel()
	fits := strings.Repeat("a", 58)    // fake__ + 58 = 64 characters
	tooLong := strings.Repeat("b", 59) // fake__ + 59 = 65 characters
	entry := fakeUpstreamEntry(t, envFakeExtraTools+"="+strings.Join([]string{fits, tooLong, "has.dot"}, ","))
	g := startGateway(t, writeConfig(t, map[string]any{"fake": entry}), nil)

	g.wantTools(t, "fake__"+fits, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", "upstream=fake", "fake__"+tooLong)
	g.wantLogLine(t, "WARN", "upstream=fake", "fake__has.dot")
	g.wantLogLine(t, "INFO", "upstream=fake", "tools=3")
}

func TestServeWithNoEnabledUpstreamsServesZeroTools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		upstreams map[string]any
	}{
		{name: "empty mcpServers", upstreams: map[string]any{}},
		{name: "all disabled", upstreams: map[string]any{
			"off": map[string]any{"command": "sprut-test-no-such-command", "disabled": true},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := startGateway(t, writeConfig(t, tt.upstreams), nil)

			g.wantTools(t)
			g.wantLogLine(t, "INFO", "ready=0", "failed=0", "tools=0")
			g.closeAgent(t)
		})
	}
}

func TestServeRoutesEachNamespacedToolToItsOwnUpstream(t *testing.T) {
	t.Parallel()
	upstreams := map[string]any{}
	for _, name := range []string{"one", "two"} {
		upstreams[name] = fakeUpstreamEntry(t, envFakeExtraTools+"=whoami", envFakeID+"=upstream "+name)
	}
	g := startGateway(t, writeConfig(t, upstreams), nil)

	g.wantTools(t, "one__echo", "one__fail", "one__whoami", "two__echo", "two__fail", "two__whoami")
	for tool, want := range map[string]string{"one__whoami": "upstream one", "two__whoami": "upstream two"} {
		res, err := g.Agent.CallTool(context.Background(), &mcp.CallToolParams{Name: tool})
		if err != nil {
			t.Fatalf("tools/call %s: %v", tool, err)
		}
		if len(res.Content) != 1 {
			t.Fatalf("tools/call %s: content = %v, want one text", tool, res.Content)
		}
		if text, ok := res.Content[0].(*mcp.TextContent); !ok || text.Text != want {
			t.Errorf("tools/call %s answered by %v, want %q", tool, res.Content[0], want)
		}
	}
}

func TestServeSkipsToolWithoutAnObjectInputSchema(t *testing.T) {
	t.Parallel()
	g := startGateway(t, writeConfig(t, map[string]any{"fake": fakeUpstreamEntry(t, envFakeBadSchemaTool+"=bad")}), nil)

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", "upstream=fake", "fake__bad")
}

func TestServeRejectsNonPositiveStartupTimeoutAsUsageError(t *testing.T) {
	t.Parallel()
	config := writeConfig(t, map[string]any{})
	for _, timeout := range []string{"0s", "-1s"} {
		t.Run(timeout, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := runSprut(t, nil, "serve", "-c", config, "--startup-timeout", timeout)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr, "startup-timeout") {
				t.Errorf("stderr does not name the flag:\n%s", stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestServeDryRunWithHealthyUpstreamsExits0AndStopsThem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	upstreams := map[string]any{}
	for _, name := range []string{"one", "two"} {
		upstreams[name] = fakeUpstreamEntry(t, envFakePIDFile+"="+filepath.Join(dir, name+".pid"))
	}

	code, stdout, stderr := runSprut(t, []string{envFakeUpstream + "=1"}, "serve", "--dry-run", "-c", writeConfig(t, upstreams))
	if code != 0 {
		t.Errorf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "ready=2 failed=0 tools=4") {
		t.Errorf("no startup summary for two ready Upstreams; stderr:\n%s", stderr)
	}
	for _, name := range []string{"one", "two"} {
		wantUpstreamGone(t, filepath.Join(dir, name+".pid"))
	}
}

func TestServeDryRunExits1WhenAnyUpstreamFailsOrTimesOut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		startup string
	}{
		{name: "fails", startup: "fail"},
		{name: "times out", startup: "hang"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := writeConfig(t, map[string]any{
				"fake":   fakeUpstreamEntry(t),
				"broken": fakeUpstreamEntry(t, envFakeStartup+"="+tt.startup),
			})

			code, stdout, stderr := runSprut(t, []string{envFakeUpstream + "=1"},
				"serve", "--dry-run", "--startup-timeout", "300ms", "-c", config)
			if code != 1 {
				t.Errorf("exit code = %d, want 1\nstderr:\n%s", code, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "ready=1 failed=1") {
				t.Errorf("no startup summary with the failed Upstream; stderr:\n%s", stderr)
			}
		})
	}
}

func TestServeDryRunLogsExactlyWhatANormalStartupLogs(t *testing.T) {
	t.Parallel()
	config := writeConfig(t, map[string]any{
		"fake":     fakeUpstreamEntry(t, envFakeExtraTools+"=has.dot"),
		"failing":  fakeUpstreamEntry(t, envFakeStartup+"=fail"),
		"unset":    fakeUpstreamEntry(t, "TOKEN=${SPRUT_TEST_UNSET}"),
		"disabled": map[string]any{"command": "x", "disabled": true},
	})
	env := []string{envFakeUpstream + "=1"}

	// A normal run whose Agent goes away at once (EOF on stdin) logs startup
	// and nothing else.
	_, _, normal := runSprut(t, env, "serve", "-c", config)
	_, _, dryRun := runSprut(t, env, "serve", "--dry-run", "-c", config)
	if !strings.Contains(normal, `msg="gateway started"`) {
		t.Fatalf("normal run logged no startup summary:\n%s", normal)
	}

	if got, want := withoutTimes(dryRun), withoutTimes(normal); !slices.Equal(got, want) {
		t.Errorf("dry run log:\n%s\nwant the normal startup log:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// withoutTimes returns the lines of a log with their time= field removed.
func withoutTimes(log string) []string {
	var out []string
	for _, line := range lines(log) {
		_, rest, _ := strings.Cut(line, " ")
		out = append(out, rest)
	}
	return out
}

func TestServeVerboseLogsEveryNamespacedToolAndToolCallAtDebug(t *testing.T) {
	t.Parallel()
	upstreams := map[string]any{"one": fakeUpstreamEntry(t), "two": fakeUpstreamEntry(t)}
	g := startGateway(t, writeConfig(t, upstreams), nil, "-v")

	for _, upstream := range []string{"one", "two"} {
		for _, tool := range []string{"echo", "fail"} {
			g.wantLogLine(t, "DEBUG", "upstream="+upstream, "tool="+upstream+"__"+tool)
		}
	}

	ctx := context.Background()
	for _, tool := range []string{"two__echo", "two__fail"} {
		if _, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"text": "x"}}); err != nil {
			t.Fatalf("tools/call %s: %v", tool, err)
		}
	}
	g.wantLogLine(t, "DEBUG", "upstream=two", "tool=two__echo", "duration=", "isError=false")
	g.wantLogLine(t, "DEBUG", "upstream=two", "tool=two__fail", "duration=", "isError=true")
}
