package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/mcptest"
)

func TestServeListsUpstreamToolsAsNamespacedTools(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)

	res, err := g.Agent.ListTools(t.Context(), nil)
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
			"inputSchema":  mcptest.EchoInputSchema,
			"outputSchema": mcptest.EchoOutputSchema,
		}),
		"fake__fail": toJSON(t, map[string]any{
			"name":        "fake__fail",
			"description": "Always returns an isError result.",
			"inputSchema": map[string]any{"type": "object"},
		}),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("tools/list (-want +got):\n%s", diff)
	}
}

func TestServeRoutesToolCallsToUpstreamAndReturnsResultUnchanged(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)
	ctx := t.Context()

	tests := []struct {
		tool string
		args map[string]any
		want map[string]any
	}{
		{
			tool: "fake__echo",
			args: map[string]any{"text": "hello"},
			want: mcptest.EchoResult("hello"),
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
			if diff := cmp.Diff(toJSON(t, tt.want), callResult(t, res)); diff != "" {
				t.Errorf("result (-want +got):\n%s", diff)
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
	if diff := cmp.Diff(toJSON(t, map[string]any{"tools": map[string]any{}}), toJSON(t, init.Capabilities)); diff != "" {
		t.Errorf("capabilities (-want +got):\n%s", diff)
	}
}

func TestServeLogsLogfmtAtInfoToStderrAndWritesOnlyMCPToStdout(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)
	ctx := t.Context()
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
	g.wantLogLine(t, "INFO", `msg="upstream ready"`, "upstream=fake", "tools=2", "protocol=", "duration=")
}

func TestServeInterpolatesVariablesIntoUpstreamEnvironmentOverInheritedOne(t *testing.T) {
	t.Parallel()
	envFile := filepath.Join(t.TempDir(), "upstream.env")
	entry := mcptest.Stdio{EnvFile: envFile}.Entry(t, "SPRUT_TEST_SECRET=token-${SECRET}", "SPRUT_TEST_OVERRIDE=from-config")
	g := startGateway(t, writeConfig(t, map[string]any{"fake": entry}), []string{
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
		if !slices.Equal(got[k], []string{want}) {
			t.Errorf("Upstream environment %s = %q, want [%q]", k, got[k], want)
		}
	}
}

func TestServeSkipsUpstreamWithUnsetVariableAndServesTheOthers(t *testing.T) {
	t.Parallel()
	broken := mcptest.Stdio{}.Entry(t, "TOKEN=${SPRUT_TEST_UNSET}")
	g := startGateway(t, writeConfig(t, map[string]any{"fake": mcptest.Stdio{}.Entry(t), "broken": broken}), nil)

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

func TestServeExits1WhenReadingFromTheAgentFails(t *testing.T) {
	t.Parallel()
	code, _, stderr := runSprutOn(t, failingReader{}, nil, "serve", "-c", writeConfig(t, map[string]any{}))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, errAgentRead.Error()) {
		t.Errorf("stderr does not contain %q:\n%s", errAgentRead, stderr)
	}
}

var errAgentRead = errors.New("agent read failed")

// failingReader is an Agent whose stdin fails with something other than EOF.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errAgentRead }

func TestServeExcludesUpstreamThatFailsAtStartupAndServesTheOthers(t *testing.T) {
	t.Parallel()
	failing := mcptest.Stdio{Startup: mcptest.Fail}.Entry(t)
	g := startGateway(t, writeConfig(t, map[string]any{"fake": mcptest.Stdio{}.Entry(t), "failing": failing}), nil)

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", "upstream=failing", "err=")
	g.wantLogLine(t, "INFO", "ready=1", "failed=1", "tools=2")
	g.closeAgent(t)
}

func TestServeExcludesUpstreamThatHangsPastStartupTimeout(t *testing.T) {
	t.Parallel()
	hanging := mcptest.Stdio{Startup: mcptest.Hang}.Entry(t)
	begin := time.Now()
	g := startGateway(t, writeConfig(t, map[string]any{"fake": mcptest.Stdio{}.Entry(t), "hanging": hanging}), nil,
		"--startup-timeout", "1s")
	// Well under the ~5s grace a hung Upstream gets to exit once stopped:
	// stopping it must not hold up startup.
	if took := time.Since(begin); took > 3*time.Second {
		t.Errorf("startup took %s with a 1s startup timeout", took)
	}

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", "upstream=hanging", "err=", "1s")
	g.closeAgent(t)
}

func TestServeStartsUpstreamsConcurrently(t *testing.T) {
	t.Parallel()
	// Each Upstream waits for all three to be running before it answers, so
	// started one after another the first would never become ready.
	meet := mcptest.Stdio{Rendezvous: &mcptest.Rendezvous{N: 3, Dir: t.TempDir()}}
	g := startGateway(t, writeConfig(t, map[string]any{
		"one":   meet.Entry(t),
		"two":   meet.Entry(t),
		"three": meet.Entry(t),
	}), nil, "--startup-timeout", "5s")

	g.wantTools(t, "one__echo", "one__fail", "three__echo", "three__fail", "two__echo", "two__fail")
}

func TestServeSkipsToolWhoseNamespacedNameBreaksTheNameRules(t *testing.T) {
	t.Parallel()
	fits := strings.Repeat("a", 58)    // fake__ + 58 = 64 characters
	tooLong := strings.Repeat("b", 59) // fake__ + 59 = 65 characters
	entry := mcptest.Stdio{ExtraTools: []string{fits, tooLong, "has.dot"}}.Entry(t)
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
		upstreams[name] = mcptest.Stdio{ExtraTools: []string{"whoami"}, ID: "upstream " + name}.Entry(t)
	}
	g := startGateway(t, writeConfig(t, upstreams), nil)

	g.wantTools(t, "one__echo", "one__fail", "one__whoami", "two__echo", "two__fail", "two__whoami")
	for tool, want := range map[string]string{"one__whoami": "upstream one", "two__whoami": "upstream two"} {
		res, err := g.Agent.CallTool(t.Context(), &mcp.CallToolParams{Name: tool})
		if err != nil {
			t.Fatalf("tools/call %s: %v", tool, err)
		}
		if got := resultText(t, res); got != want {
			t.Errorf("tools/call %s answered by %q, want %q", tool, got, want)
		}
	}
}

func TestServeSkipsToolWithoutAnObjectInputSchema(t *testing.T) {
	t.Parallel()
	g := startGateway(t, writeConfig(t, map[string]any{"fake": mcptest.Stdio{BadSchemaTool: "bad"}.Entry(t)}), nil)

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", "upstream=fake", "fake__bad")
}

func TestServeDryRunWithHealthyUpstreamsExits0AndStopsThem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	upstreams := map[string]any{}
	for _, name := range []string{"one", "two"} {
		upstreams[name] = mcptest.Stdio{PIDFile: filepath.Join(dir, name+".pid")}.Entry(t)
	}

	code, stdout, stderr := runSprut(t, nil, "serve", "--dry-run", "-c", writeConfig(t, upstreams))
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "ready=2 failed=0 tools=4") {
		t.Errorf("no startup summary for two ready Upstreams")
	}
	for _, name := range []string{"one", "two"} {
		wantGone(t, filepath.Join(dir, name+".pid"))
	}
}

func TestServeDryRunExits1WhenAnyUpstreamFailsOrTimesOut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		startup mcptest.Startup
	}{
		{name: "fails", startup: mcptest.Fail},
		{name: "times out", startup: mcptest.Hang},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := writeConfig(t, map[string]any{
				"fake":   mcptest.Stdio{}.Entry(t),
				"broken": mcptest.Stdio{Startup: tt.startup}.Entry(t),
			})

			code, stdout, stderr := runSprut(t, nil,
				"serve", "--dry-run", "--startup-timeout", "1s", "-c", config)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if !strings.Contains(stderr, "ready=1 failed=1") {
				t.Errorf("no startup summary with the failed Upstream")
			}
		})
	}
}

func TestServeDryRunLogsExactlyWhatANormalStartupLogs(t *testing.T) {
	t.Parallel()
	config := writeConfig(t, map[string]any{
		"fake":     mcptest.Stdio{ExtraTools: []string{"has.dot"}}.Entry(t),
		"failing":  mcptest.Stdio{Startup: mcptest.Fail}.Entry(t),
		"unset":    mcptest.Stdio{}.Entry(t, "TOKEN=${SPRUT_TEST_UNSET}"),
		"disabled": map[string]any{"command": "x", "disabled": true},
	})

	// A normal run whose Agent goes away at once (EOF on stdin) logs startup
	// and nothing else.
	_, _, normal := runSprut(t, nil, "serve", "-c", config)
	_, _, dryRun := runSprut(t, nil, "serve", "--dry-run", "-c", config)
	if !strings.Contains(normal, `msg="gateway started"`) {
		t.Fatalf("normal run logged no startup summary:\n%s", normal)
	}

	if diff := cmp.Diff(withoutTimes(normal), withoutTimes(dryRun)); diff != "" {
		t.Errorf("dry run log, against the normal startup log (-normal +dry run):\n%s", diff)
	}
}

// withoutTimes returns the lines of a log with their time= and duration=
// fields removed.
func withoutTimes(log string) []string {
	var out []string
	for _, line := range lines(log) {
		_, rest, _ := strings.Cut(line, " ")
		out = append(out, durationField.ReplaceAllString(rest, ""))
	}
	return out
}

var durationField = regexp.MustCompile(` duration=\S+`)

func TestServeVerboseLogsEveryNamespacedToolAndToolCallAtDebug(t *testing.T) {
	t.Parallel()
	upstreams := map[string]any{"one": mcptest.Stdio{}.Entry(t), "two": mcptest.Stdio{}.Entry(t)}
	g := startGateway(t, writeConfig(t, upstreams), nil, "-v")

	for _, upstream := range []string{"one", "two"} {
		for _, tool := range []string{"echo", "fail"} {
			g.wantLogLine(t, "DEBUG", "upstream="+upstream, "tool="+upstream+"__"+tool)
		}
	}

	ctx := t.Context()
	for _, tool := range []string{"two__echo", "two__fail"} {
		if _, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"text": "x"}}); err != nil {
			t.Fatalf("tools/call %s: %v", tool, err)
		}
	}
	g.wantLogLine(t, "DEBUG", "upstream=two", "tool=two__echo", "duration=", "isError=false")
	g.wantLogLine(t, "DEBUG", "upstream=two", "tool=two__fail", "duration=", "isError=true")
}

func TestServeLogsUpstreamStderrLineByLine(t *testing.T) {
	t.Parallel()
	entry := mcptest.Stdio{Stderr: "first line\nsecond line\n"}.Entry(t)
	g := startGateway(t, writeConfig(t, map[string]any{"fake": entry}), nil)
	g.closeAgent(t)

	g.wantLogLine(t, "INFO", `msg="upstream stderr"`, "upstream=fake", `line="first line"`)
	g.wantLogLine(t, "INFO", `msg="upstream stderr"`, "upstream=fake", `line="second line"`)
}

func TestServeKillsUpstreamsThatIgnoreSIGTERMAfterOneGrace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	upstreams := map[string]any{}
	for _, name := range []string{"one", "two"} {
		upstreams[name] = mcptest.Stdio{IgnoreSIGTERM: true, PIDFile: filepath.Join(dir, name+".pid")}.Entry(t)
	}
	g := startGateway(t, writeConfig(t, upstreams), nil)

	begin := time.Now()
	g.closeAgent(t)
	// About 5s of grace after SIGTERM, then SIGKILL, for all Upstreams at once.
	if took := time.Since(begin); took < 4*time.Second || took > 9*time.Second {
		t.Errorf("shutdown took %s, want about 5s", took)
	}
	for _, name := range []string{"one", "two"} {
		wantGone(t, filepath.Join(dir, name+".pid"))
	}
}

func TestServeGivesUpOnAnUpstreamThatNeverExits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile, groupFile := filepath.Join(dir, "upstream.pid"), filepath.Join(dir, "group.pid")
	entry := mcptest.Stdio{PIDFile: pidFile, LeaveGroupPIDFile: groupFile}.Entry(t)
	g := startGateway(t, writeConfig(t, map[string]any{"fake": entry}), nil)
	// What the Gateway gives up on is left for the test to stop.
	t.Cleanup(func() { _ = syscall.Kill(-readPID(t, groupFile), syscall.SIGKILL) })

	begin := time.Now()
	g.closeAgent(t)
	// Its group is gone at once, so shutdown only waits for it to be reaped:
	// about 2s, the gateway package's exitGrace.
	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("shutdown took %s, want about 2s", took)
	}
	g.wantLogLine(t, "WARN", `msg="upstream did not stop"`, "upstream=fake", "pid="+strconv.Itoa(readPID(t, pidFile)))
}

func TestServeShutsDownCleanlyLeavingNoUpstreamOrGrandchildBehind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		stop func(*gateway)
	}{
		{name: "EOF on stdin", stop: func(g *gateway) { _ = g.Agent.Close() }},
		{name: "a signal", stop: func(g *gateway) { g.sendSignal() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			pidFile, grandchildPIDFile := filepath.Join(dir, "upstream.pid"), filepath.Join(dir, "grandchild.pid")
			entry := mcptest.Stdio{PIDFile: pidFile, GrandchildPIDFile: grandchildPIDFile}.Entry(t)
			g := startGateway(t, writeConfig(t, map[string]any{"fake": entry}), nil)

			tt.stop(g)
			g.wantCleanExit(t, tt.name)
			g.wantNoLogLine(t, "WARN", `msg="upstream exited"`)
			wantGone(t, pidFile)
			wantGone(t, grandchildPIDFile)
		})
	}
}

func TestServeDoesNotReportTheExitOfAnUpstreamThatFailsAtStartup(t *testing.T) {
	t.Parallel()
	upstreams := map[string]any{"fake": mcptest.Stdio{}.Entry(t), "failing": mcptest.Stdio{Startup: mcptest.Fail}.Entry(t)}
	g := startGateway(t, writeConfig(t, upstreams), nil)

	g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=failing")
	g.closeAgent(t)
	g.wantNoLogLine(t, "WARN", `msg="upstream exited"`)
}

func TestServeReportsTheExitOfAnUpstreamThatHangsUpRightAfterListingItsTools(t *testing.T) {
	t.Parallel()
	upstreams := map[string]any{"quitter": mcptest.Stdio{HangUpAfterList: true}.Entry(t)}
	g := startGateway(t, writeConfig(t, upstreams), nil)

	g.wantTools(t, "quitter__echo", "quitter__fail")
	// Closing the Agent before the Gateway notices the Upstream is gone
	// would have the Gateway end the session first, which is not reported.
	exit := []string{`msg="upstream exited"`, "upstream=quitter", `err="exit status 1"`}
	if !eventually(func() bool { return g.logged("WARN", exit...) }) {
		t.Fatalf("no WARN line containing all of %q", exit)
	}
	g.closeAgent(t)
}

func TestServeAnswersCallsToUnknownToolsWithAnErrorResult(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)

	for _, tool := range []string{"fake__nope", "nobody__echo", "echo"} {
		res, err := g.Agent.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"text": "x"}})
		if err != nil {
			t.Fatalf("tools/call %s: %v, want an isError result", tool, err)
		}
		if text := resultText(t, res); !res.IsError || !strings.Contains(text, "unknown tool") || !strings.Contains(text, tool) {
			t.Errorf("tools/call %s = %q (isError %v), want an isError result naming the unknown tool", tool, text, res.IsError)
		}
	}
}

func TestServeAnswersCallsToACrashedUpstreamWithAnErrorResultAndKeepsServing(t *testing.T) {
	t.Parallel()
	upstreams := map[string]any{"doomed": mcptest.Stdio{CrashTool: true}.Entry(t), "fake": mcptest.Stdio{}.Entry(t)}
	g := startGateway(t, writeConfig(t, upstreams), nil, "-v")
	ctx := t.Context()

	// The call that crashes the Upstream, then calls to the dead Upstream:
	// it is not restarted.
	for _, tool := range []string{"doomed__crash", "doomed__echo", "doomed__echo"} {
		res, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"text": "x"}})
		if err != nil {
			t.Fatalf("tools/call %s: %v, want an isError result", tool, err)
		}
		if text := resultText(t, res); !res.IsError || !strings.Contains(text, `"doomed"`) {
			t.Errorf("tools/call %s = %q (isError %v), want an isError result naming the Upstream", tool, text, res.IsError)
		}
	}

	g.wantLogLine(t, "DEBUG", "tool=doomed__crash", "isError=true")

	res, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "fake__echo", Arguments: map[string]any{"text": "x"}})
	if err != nil || res.IsError {
		t.Errorf("tools/call fake__echo after another Upstream crashed = %v, %v; want a result", res, err)
	}
	g.closeAgent(t)
	g.wantLogLine(t, "WARN", `msg="upstream exited"`, "upstream=doomed", `err="exit status 1"`)
}

func TestServeEndsCallsToAnUpstreamThatExitsWhileItsChildHoldsStdoutAndStopsTheChild(t *testing.T) {
	t.Parallel()
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	upstreams := map[string]any{"orphaner": mcptest.Stdio{OrphanToolPIDFile: childPIDFile}.Entry(t)}
	g := startGateway(t, writeConfig(t, upstreams), nil)
	// Bounds the test, not the call: a call that is not ended promptly
	// fails the timing check below.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	begin := time.Now()
	res, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "orphaner__orphan"})
	if err != nil {
		t.Fatalf("tools/call orphaner__orphan: %v, want an isError result", err)
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Errorf("tools/call orphaner__orphan took %s, want it ended within a few seconds of the Upstream exiting", took)
	}
	if text := resultText(t, res); !res.IsError || !strings.Contains(text, `"orphaner"`) {
		t.Errorf("tools/call orphaner__orphan = %q (isError %v), want an isError result naming the Upstream", text, res.IsError)
	}
	waitForExit(t, childPIDFile)

	g.closeAgent(t)
	g.wantLogLine(t, "WARN", `msg="upstream exited"`, "upstream=orphaner", `err="exit status 1"`)
}

func TestServePassesUpstreamProtocolErrorsThrough(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)

	// The fake's echo answers arguments it can't decode with a protocol
	// error, not an isError result.
	res, err := g.Agent.CallTool(t.Context(), &mcp.CallToolParams{Name: "fake__echo", Arguments: map[string]any{"text": 5}})
	if err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("tools/call = %v, %v; want the Upstream's protocol error", res, err)
	}
}

func TestServeCancelsCallOnUpstreamWhenAgentCancelsIt(t *testing.T) {
	t.Parallel()
	progress := filepath.Join(t.TempDir(), "block")
	g := startGateway(t, writeConfig(t, map[string]any{"fake": mcptest.Stdio{BlockToolFile: progress}.Entry(t)}), nil)

	ctx, cancel := context.WithCancel(t.Context())
	called := g.startBlockingCall(ctx, t, progress)
	cancel() // the Agent sends notifications/cancelled

	if err := <-called; err == nil {
		t.Error("cancelled tools/call returned no error")
	}
	waitForFile(t, progress, "cancelled")
	g.closeAgent(t)
}

func TestServeForwardsConcurrentCallsIncludingSeveralToOneUpstream(t *testing.T) {
	t.Parallel()
	// Each Upstream answers only once three calls are in flight to it, so
	// calls forwarded one at a time would never be answered.
	const perUpstream = 3
	gather := mcptest.Stdio{GatherTool: perUpstream}
	upstreams := map[string]any{"one": gather.Entry(t), "two": gather.Entry(t)}
	g := startGateway(t, writeConfig(t, upstreams), nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, upstream := range []string{"one", "two"} {
		for range perUpstream {
			tool := upstream + "__gather"
			wg.Go(func() {
				res, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: tool})
				if err != nil || res.IsError {
					t.Errorf("tools/call %s = %v, %v", tool, res, err)
				}
			})
		}
	}
	wg.Wait()
}

func TestServeImposesNoTimeoutOnToolCalls(t *testing.T) {
	t.Parallel()
	progress := filepath.Join(t.TempDir(), "block")
	entry := mcptest.Stdio{BlockToolFile: progress}.Entry(t)
	// The startup timeout is the Gateway's only timeout; it must not apply
	// to calls.
	g := startGateway(t, writeConfig(t, map[string]any{"fake": entry}), nil, "--startup-timeout", "1s")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := g.startBlockingCall(ctx, t, progress)

	select {
	case err := <-called:
		t.Fatalf("tools/call ended on its own: %v", err)
	case <-time.After(3 * time.Second): // three startup timeouts
	}
	if data, err := os.ReadFile(progress); err != nil || string(data) != "started" {
		t.Errorf("Upstream's call: %s holds %q (err %v), want it still started", progress, data, err)
	}

	// The Upstream writes progress once more when the call ends, which must
	// happen before the test's temp dir is removed.
	cancel()
	<-called
	waitForFile(t, progress, "cancelled")
	g.closeAgent(t)
}

func TestServeBridgesProtocolErasBetweenAgentAndUpstream(t *testing.T) {
	t.Parallel()
	eras := []string{mcptest.Legacy, mcptest.Modern}
	for _, agent := range eras {
		for _, upstream := range eras {
			t.Run("Agent "+agent+" Upstream "+upstream, func(t *testing.T) {
				t.Parallel()
				path := writeConfig(t, map[string]any{"fake": mcptest.Stdio{Protocol: upstream}.Entry(t)})
				g := startGatewayAs(t, agent, path, nil)

				if got := g.Agent.InitializeResult().ProtocolVersion; got != agent {
					t.Errorf("Agent negotiated %s, want %s", got, agent)
				}
				g.wantLogLine(t, "INFO", `msg="upstream ready"`, "upstream=fake", "protocol="+upstream)
				g.wantTools(t, "fake__echo", "fake__fail")
				g.wantLogLine(t, "INFO", `msg="agent connected"`, "protocol="+agent)
				res, err := g.Agent.CallTool(t.Context(),
					&mcp.CallToolParams{Name: "fake__echo", Arguments: map[string]any{"text": "hi"}})
				if err != nil {
					t.Fatalf("tools/call: %v", err)
				}
				if diff := cmp.Diff(toJSON(t, mcptest.EchoResult("hi")), callResult(t, res)); diff != "" {
					t.Errorf("result (-want +got):\n%s", diff)
				}
			})
		}
	}
}

func TestServeEndsCallWithAnErrorResultWhenModernUpstreamAsksForInteractiveInput(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, map[string]any{
		"fake": mcptest.Stdio{Protocol: mcptest.Modern, AskTool: true}.Entry(t),
	})
	g := startGateway(t, path, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	res, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "fake__ask"})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if text := resultText(t, res); !res.IsError || !strings.Contains(text, "interactive input is not supported by the gateway") {
		t.Errorf("result = %q (isError %v), want an isError result saying interactive input is not supported by the Gateway", text, res.IsError)
	}
	g.wantTools(t, "fake__ask", "fake__echo", "fake__fail") // still serving
}

func TestServeServesHTTPUpstreamToolsAndRoutesCallsToIt(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{mcptest.Legacy, mcptest.Modern} {
		t.Run("Upstream "+protocol, func(t *testing.T) {
			t.Parallel()
			remote := mcptest.StartHTTP(t, protocol)
			g := startGateway(t, writeConfig(t, map[string]any{
				"fake":   mcptest.Stdio{}.Entry(t),
				"remote": map[string]any{"url": remote.URL},
			}), nil)

			g.wantLogLine(t, "INFO", `msg="upstream ready"`, "upstream=remote", "tools=2", "protocol="+protocol)
			g.wantTools(t, "fake__echo", "fake__fail", "remote__echo", "remote__fail")
			res, err := g.Agent.CallTool(t.Context(),
				&mcp.CallToolParams{Name: "remote__echo", Arguments: map[string]any{"text": "hi"}})
			if err != nil {
				t.Fatalf("tools/call: %v", err)
			}
			if diff := cmp.Diff(toJSON(t, mcptest.EchoResult("hi")), callResult(t, res)); diff != "" {
				t.Errorf("result (-want +got):\n%s", diff)
			}
			g.closeAgent(t)
		})
	}
}

// The request never gets an answer, which the SDK reports as a JSON-RPC
// error of its own; it is not the Upstream's answer to pass on.
func TestServeAnswersCallsToAnHTTPUpstreamThatIsGoneWithAnErrorResult(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{mcptest.Legacy, mcptest.Modern} {
		t.Run("Upstream "+protocol, func(t *testing.T) {
			t.Parallel()
			remote := mcptest.StartHTTP(t, protocol)
			g := startGateway(t, writeConfig(t, map[string]any{"remote": map[string]any{"url": remote.URL}}), nil)
			g.wantTools(t, "remote__echo", "remote__fail")

			remote.Close()
			res, err := g.Agent.CallTool(t.Context(),
				&mcp.CallToolParams{Name: "remote__echo", Arguments: map[string]any{"text": "hi"}})

			if err != nil {
				t.Fatalf("tools/call: %v, want an isError result", err)
			}
			if text := resultText(t, res); !res.IsError || !strings.Contains(text, `call to upstream "remote" failed`) {
				t.Errorf("tools/call = %q (isError %v), want an isError result naming the Upstream", text, res.IsError)
			}
			g.closeAgent(t)
		})
	}
}

func TestServeSendsConfiguredHeadersOnEveryHTTPRequest(t *testing.T) {
	t.Parallel()
	remote := mcptest.StartHTTP(t, "")
	g := startGateway(t, writeConfig(t, map[string]any{"remote": map[string]any{
		"url":     remote.URL,
		"headers": map[string]string{"Authorization": "Bearer ${TOKEN}", "X-Static": "yes"},
	}}), []string{"TOKEN=secret"})

	if _, err := g.Agent.CallTool(t.Context(),
		&mcp.CallToolParams{Name: "remote__echo", Arguments: map[string]any{"text": "hi"}}); err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	g.closeAgent(t)

	requests := remote.Headers()
	// At least discover or initialize, tools/list and tools/call.
	if len(requests) < 3 {
		t.Fatalf("Upstream got %d requests, want at least 3", len(requests))
	}
	for i, h := range requests {
		if got := h.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("request %d: Authorization = %q, want %q", i, got, "Bearer secret")
		}
		if got := h.Get("X-Static"); got != "yes" {
			t.Errorf("request %d: X-Static = %q, want %q", i, got, "yes")
		}
	}
}

func TestServeFailsSSEOnlyHTTPUpstreamWithoutFallbackAndServesTheOthers(t *testing.T) {
	t.Parallel()
	g := startGateway(t, writeConfig(t, map[string]any{
		"fake":       mcptest.Stdio{}.Entry(t),
		"legacy-sse": map[string]any{"url": mcptest.StartSSEOnly(t)},
	}), nil)

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=legacy-sse", "speaks only the deprecated HTTP+SSE transport, which is not supported")
	g.wantLogLine(t, "INFO", "ready=1", "failed=1")
	g.closeAgent(t)
}

func TestServeExcludesHTTPUpstreamThatHangsPastStartupTimeout(t *testing.T) {
	t.Parallel()
	begin := time.Now()
	g := startGateway(t, writeConfig(t, map[string]any{
		"fake":    mcptest.Stdio{}.Entry(t),
		"hanging": map[string]any{"url": mcptest.StartHanging(t)},
	}), nil, "--startup-timeout", "1s")
	if took := time.Since(begin); took > 3*time.Second {
		t.Errorf("startup took %s with a 1s startup timeout", took)
	}

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantLogLine(t, "WARN", "upstream=hanging", "startup timed out after 1s")
	g.closeAgent(t)
}

func TestServeSendsConfiguredHeadersOnlyToTheConfiguredHost(t *testing.T) {
	t.Parallel()
	elsewhere := mcptest.StartHTTP(t, "")
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	g := startGateway(t, writeConfig(t, map[string]any{"remote": map[string]any{
		"url":     redirector.URL,
		"headers": map[string]string{"Authorization": "Bearer secret"},
	}}), nil)

	g.wantTools(t, "remote__echo", "remote__fail")
	g.closeAgent(t)
	requests := elsewhere.Headers()
	if len(requests) == 0 {
		t.Fatal("redirect target got no requests")
	}
	for i, h := range requests {
		if got := h.Get("Authorization"); got != "" {
			t.Errorf("request %d to the redirect target: Authorization = %q, want none", i, got)
		}
	}
}

func TestServeReachesHTTPUpstreamThroughTheProxyInItsEnvironment(t *testing.T) {
	t.Parallel()
	// The proxy serves the Upstream itself, so the request reaches it only
	// through the proxy: remote.invalid resolves nowhere.
	proxy := mcptest.StartHTTP(t, "")
	g := startGateway(t, writeConfig(t, map[string]any{
		"remote": map[string]any{"url": "http://remote.invalid/mcp"},
	}), []string{"HTTP_PROXY=" + proxy.URL})

	g.wantTools(t, "remote__echo", "remote__fail")
	g.closeAgent(t)
}

func TestServeBlamesHTTPSSEOnlyOnAServerThatSpeaksIt(t *testing.T) {
	t.Parallel()
	g := startGateway(t, writeConfig(t, map[string]any{"gone": map[string]any{"url": mcptest.RefusingURL()}}), nil)

	g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=gone", "connection refused")
	g.wantNoLogLine(t, "WARN", "HTTP+SSE")
	g.closeAgent(t)
}

func TestServeDoesNotBlameHTTPSSEOnAStreamableHTTPServerWithASessionlessGETStream(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		chunk string
		// The stream never ends, so only a bound on the probe stops it
		// before the startup timeout. A flood is cut off by size, well
		// before the probe's time runs out.
		maxStartup time.Duration
	}{
		{"silent", "", 10 * time.Second},
		{"one endless event", "data: " + strings.Repeat("x", 1000) + "\n", time.Second},
		{"endless keep-alives", ":\n\n", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			begin := time.Now()
			g := startGateway(t, writeConfig(t, map[string]any{
				"broken": map[string]any{"url": mcptest.StartGETStream(t, tc.chunk)},
			}), nil, "--startup-timeout", "20s")
			if took := time.Since(begin); took > tc.maxStartup {
				t.Errorf("startup took %s", took)
			}

			g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=broken", "Internal Server Error")
			g.wantNoLogLine(t, "WARN", "HTTP+SSE")
			g.closeAgent(t)
		})
	}
}

func TestServeListsUpstreamsSkippedAtStartupInItsInstructions(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{mcptest.Legacy, mcptest.Modern} {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()
			g := startGatewayAs(t, protocol, writeConfig(t, map[string]any{
				"fake":    mcptest.Stdio{}.Entry(t),
				"failing": mcptest.Stdio{Startup: mcptest.Fail}.Entry(t),
				"hanging": mcptest.Stdio{Startup: mcptest.Hang}.Entry(t),
			}), nil, "--startup-timeout", "1s")

			instructions := g.Agent.InitializeResult().Instructions
			for _, want := range []string{"failing", "hanging", "startup timed out after 1s"} {
				if !strings.Contains(instructions, want) {
					t.Errorf("instructions do not mention %q:\n%s", want, instructions)
				}
			}
			if strings.Contains(instructions, "fake") {
				t.Errorf("instructions mention the Upstream that started:\n%s", instructions)
			}
			g.closeAgent(t)
		})
	}
}

func TestServeHasEmptyInstructionsWhenNoUpstreamWasSkipped(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t)
	if got := g.Agent.InitializeResult().Instructions; got != "" {
		t.Errorf("instructions = %q, want empty", got)
	}
	g.closeAgent(t)
}

func TestServeKeepsConfigSecretsOutOfItsInstructions(t *testing.T) {
	t.Parallel()
	// Refusing connections, so the reason quotes the URL.
	withPassword := strings.Replace(mcptest.RefusingURL(), "://", "://user:url-secret@", 1)
	g := startGateway(t, writeConfig(t, map[string]any{
		"fake":   mcptest.Stdio{}.Entry(t),
		"remote": map[string]any{"url": withPassword, "headers": map[string]string{"Authorization": "Bearer ${TOKEN}"}},
		"local":  mcptest.Stdio{Startup: mcptest.Fail}.Entry(t, "API_KEY=${KEY}"),
	}), []string{"TOKEN=header-secret", "KEY=env-secret"})

	instructions := g.Agent.InitializeResult().Instructions
	for _, name := range []string{"remote", "local"} {
		if !strings.Contains(instructions, name) {
			t.Errorf("instructions do not mention %q:\n%s", name, instructions)
		}
	}
	wantNoSecrets(t, "instructions", instructions, "url-secret", "header-secret", "env-secret")
	g.closeAgent(t)
}

// serve logs the absolute path of the Config it reads, wherever it found
// it, so that the user can tell which file an Agent's sprut uses.
func TestServeLogsWhereItReadsTheConfig(t *testing.T) {
	t.Parallel()
	upstreams := map[string]any{"fake": mcptest.Stdio{}.Entry(t)}
	configHome := t.TempDir()
	atDefault := filepath.Join(configHome, "sprut", "config.json")
	writeConfigAt(t, atDefault, upstreams)
	abs := writeConfig(t, upstreams)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		env  []string
		args []string
		want string
	}{
		{name: "default location", env: []string{"XDG_CONFIG_HOME=" + configHome}, want: atDefault},
		{name: "relative -c", args: []string{"-c", rel}, want: abs},
		{name: "SPRUT_CONFIG", env: []string{"SPRUT_CONFIG=" + abs}, want: abs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code, _, stderr := runSprut(t, tc.env, append([]string{"serve", "--dry-run"}, tc.args...)...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if !regexp.MustCompile(`level=INFO msg="config loaded" path=` + regexp.QuoteMeta(tc.want) + `( |$)`).MatchString(stderr) {
				t.Errorf("no INFO line saying the config was loaded from %s", tc.want)
			}
		})
	}
}
