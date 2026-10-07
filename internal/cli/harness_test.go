package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/cli"
)

// Fake stdio Upstreams are this test binary re-executing itself. When
// envFakeUpstream is set, TestMain runs an MCP server on stdio instead of the
// tests. Its behaviour is driven by the remaining envFake* variables.
const (
	envFakeUpstream = "SPRUT_TEST_FAKE_UPSTREAM"
	// envFakePIDFile, if set, is a path the fake Upstream writes its PID to.
	envFakePIDFile = "SPRUT_TEST_FAKE_PIDFILE"
	// envFakeEnvFile, if set, is a path the fake Upstream writes its
	// environment to, one VAR=value per line.
	envFakeEnvFile = "SPRUT_TEST_FAKE_ENVFILE"
	// envFakeStartup, if set, makes the fake Upstream misbehave at startup:
	// "fail" exits with an error before speaking MCP; "hang" never answers
	// and ignores its stdin closing, so only a signal stops it.
	envFakeStartup = "SPRUT_TEST_FAKE_STARTUP"
	// envFakeRendezvous, if set to N:DIR, makes the fake Upstream mark its
	// arrival in DIR and not answer until N Upstreams have arrived there.
	envFakeRendezvous = "SPRUT_TEST_FAKE_RENDEZVOUS"
	// envFakeExtraTools, if set, is a comma-separated list of names of
	// further tools the fake Upstream offers, each taking no arguments and
	// answering with the value of envFakeID.
	envFakeExtraTools = "SPRUT_TEST_FAKE_EXTRA_TOOLS"
	// envFakeID is what the extra tools answer with, so a test can tell
	// which Upstream answered.
	envFakeID = "SPRUT_TEST_FAKE_ID"
	// envFakeBadSchemaTool, if set, is the name of one more tool, whose input
	// schema is not an object schema.
	envFakeBadSchemaTool = "SPRUT_TEST_FAKE_BAD_SCHEMA_TOOL"
	// envFakeStderr, if set, is written to the fake Upstream's stderr at
	// startup.
	envFakeStderr = "SPRUT_TEST_FAKE_STDERR"
	// envFakeIgnoreSIGTERM, if set, makes the fake Upstream ignore SIGTERM
	// and its stdin closing, so only SIGKILL stops it.
	envFakeIgnoreSIGTERM = "SPRUT_TEST_FAKE_IGNORE_SIGTERM"
	// envFakeGrandchild, if set, is a path the fake Upstream writes the PID
	// of a grandchild to: a process it starts that shares its stderr and
	// sleeps until killed.
	envFakeGrandchild = "SPRUT_TEST_FAKE_GRANDCHILD"
	// envFakeCrashTool, if set, adds a tool "crash" that makes the fake
	// Upstream exit mid-call, without answering.
	envFakeCrashTool = "SPRUT_TEST_FAKE_CRASH_TOOL"
	// envFakeBlockTool, if set, is a path, and adds a tool "block" that
	// writes "started" to it, blocks until the call is cancelled, then
	// writes "cancelled" to it.
	envFakeBlockTool = "SPRUT_TEST_FAKE_BLOCK_TOOL"
	// envFakeGatherTool, if set to N, adds a tool "gather" that answers
	// only once N calls to it are in flight together.
	envFakeGatherTool = "SPRUT_TEST_FAKE_GATHER_TOOL"
)

// fakeLifetime bounds how long a fake Upstream or grandchild that only
// waits to be stopped stays alive: longer than any test, so a test that
// stops it proves something, but short enough that one the Gateway failed to
// stop does not linger on the machine.
const fakeLifetime = time.Minute

func TestMain(m *testing.M) {
	if os.Getenv(envFakeUpstream) != "" {
		os.Exit(runFakeUpstream())
	}
	os.Exit(m.Run())
}

// The fake Upstream's tools, as raw JSON so that tests can assert that the
// Gateway passes them through unchanged.
var (
	// objectSchema is the input schema of a tool that takes no arguments.
	objectSchema    = map[string]any{"type": "object"}
	echoInputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "text to echo"},
		},
		"required": []any{"text"},
	}
	echoOutputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"echoed": map[string]any{"type": "string"},
		},
	}
	echoTool = &mcp.Tool{
		Name:         "echo",
		Title:        "Echo",
		Description:  "Echoes its text argument back.",
		InputSchema:  echoInputSchema,
		OutputSchema: echoOutputSchema,
	}
	failTool = &mcp.Tool{
		Name:        "fail",
		Description: "Always returns an isError result.",
		InputSchema: objectSchema,
	}
)

func runFakeUpstream() int {
	if os.Getenv(envFakeIgnoreSIGTERM) != "" {
		signal.Ignore(syscall.SIGTERM)
		defer time.Sleep(fakeLifetime)
	}

	if path := os.Getenv(envFakePIDFile); path != "" {
		if err := writePID(path, os.Getpid()); err != nil {
			fmt.Fprintln(os.Stderr, "fake upstream:", err)
			return 1
		}
	}

	if path := os.Getenv(envFakeEnvFile); path != "" {
		if err := os.WriteFile(path, []byte(strings.Join(os.Environ(), "\n")), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fake upstream:", err)
			return 1
		}
	}

	if path := os.Getenv(envFakeGrandchild); path != "" {
		if err := startGrandchild(path); err != nil {
			fmt.Fprintln(os.Stderr, "fake upstream:", err)
			return 1
		}
	}

	if v := os.Getenv(envFakeStderr); v != "" {
		fmt.Fprint(os.Stderr, v)
	}

	if v := os.Getenv(envFakeRendezvous); v != "" {
		if err := rendezvous(v); err != nil {
			fmt.Fprintln(os.Stderr, "fake upstream:", err)
			return 1
		}
	}

	switch os.Getenv(envFakeStartup) {
	case "fail":
		fmt.Fprintln(os.Stderr, "fake upstream: failing at startup")
		return 1
	case "hang":
		time.Sleep(fakeLifetime)
		return 1
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "fake-upstream", Version: "0.0.1"}, nil)
	server.AddTool(echoTool, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Text string }
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Text}},
			StructuredContent: map[string]any{"echoed": args.Text},
			Meta:              mcp.Meta{"fake/trace": "abc"},
		}, nil
	})
	server.AddTool(failTool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "it failed"}},
			IsError: true,
		}, nil
	})
	for name := range strings.SplitSeq(os.Getenv(envFakeExtraTools), ",") {
		if name == "" {
			continue
		}
		server.AddTool(&mcp.Tool{Name: name, InputSchema: objectSchema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: os.Getenv(envFakeID)}}}, nil
			})
	}
	if os.Getenv(envFakeCrashTool) != "" {
		server.AddTool(&mcp.Tool{Name: "crash", InputSchema: objectSchema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				os.Exit(1)
				return nil, nil
			})
	}
	if path := os.Getenv(envFakeBlockTool); path != "" {
		server.AddTool(&mcp.Tool{Name: "block", InputSchema: objectSchema},
			func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if err := os.WriteFile(path, []byte("started"), 0o644); err != nil {
					return nil, err
				}
				<-ctx.Done()
				return nil, os.WriteFile(path, []byte("cancelled"), 0o644)
			})
	}
	if v := os.Getenv(envFakeGatherTool); v != "" {
		want, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake upstream:", err)
			return 1
		}
		server.AddTool(&mcp.Tool{Name: "gather", InputSchema: objectSchema}, gather(want))
	}
	if name := os.Getenv(envFakeBadSchemaTool); name != "" {
		// AddTool insists on an object schema, so the bad one is put into
		// the tools/list result on its way out.
		server.AddTool(&mcp.Tool{Name: name, InputSchema: objectSchema},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil })
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				res, err := next(ctx, method, req)
				if list, ok := res.(*mcp.ListToolsResult); ok {
					for i, tool := range list.Tools {
						if tool.Name == name {
							bad := *tool
							bad.InputSchema = map[string]any{"type": "string"}
							list.Tools[i] = &bad
						}
					}
				}
				return res, err
			}
		})
	}
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fake upstream:", err)
		return 1
	}
	return 0
}

// gather implements envFakeGatherTool.
func gather(want int) mcp.ToolHandler {
	var arrived atomic.Int32
	all := make(chan struct{})
	return func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if int(arrived.Add(1)) == want {
			close(all)
		}
		select {
		case <-all:
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "gathered"}}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// startGrandchild implements envFakeGrandchild.
func startGrandchild(pidFile string) error {
	// By absolute path: the fake Upstream's environment has no PATH.
	cmd := exec.Command("/bin/sleep", strconv.Itoa(int(fakeLifetime.Seconds())))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return writePID(pidFile, cmd.Process.Pid)
}

// writePID records pid in path, for wantGone.
func writePID(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}

// rendezvous implements envFakeRendezvous.
func rendezvous(v string) error {
	n, dir, _ := strings.Cut(v, ":")
	want, err := strconv.Atoi(n)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(os.Getpid())), nil, 0o644); err != nil {
		return err
	}
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(entries) >= want {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startFakeGateway runs `sprut serve` with a Config holding one Upstream,
// "fake", launched with env added to its environment.
func startFakeGateway(t *testing.T, env ...string) *gateway {
	t.Helper()
	return startGateway(t, writeConfig(t, map[string]any{"fake": fakeUpstreamEntry(t)}), env)
}

// fakeUpstreamEntry returns a Config entry that launches the fake Upstream,
// with env (VAR=value pairs) as the entry's env.
func fakeUpstreamEntry(t *testing.T, env ...string) map[string]any {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	entry := map[string]any{"command": exe, "args": []string{"-test.run=^$"}}
	if len(env) > 0 {
		vars := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			vars[k] = v
		}
		entry["env"] = vars
	}
	return entry
}

// writeConfig writes a Config with the given Upstreams and returns its path.
func writeConfig(t *testing.T, upstreams map[string]any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"mcpServers": upstreams})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// gateway is a running `sprut serve` driven in-process through cli.Run, with
// an Agent connected to it over in-memory pipes.
type gateway struct {
	Agent      *mcp.ClientSession
	stdout     *syncBuffer // everything the Gateway wrote to stdout
	stderr     *syncBuffer
	exit       chan int
	sendSignal context.CancelFunc // stands in for SIGINT or SIGTERM reaching sprut
}

// startGateway runs `sprut serve -c configPath` plus args with env and
// connects an Agent to it.
func startGateway(t *testing.T, configPath string, env []string, args ...string) *gateway {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	// main delivers signals by cancelling cli.Run's ctx.
	runCtx, sendSignal := context.WithCancel(ctx)

	agentToGateway, gatewayStdin := io.Pipe()
	gatewayStdout, gatewayToAgent := io.Pipe()
	g := &gateway{stdout: &syncBuffer{}, stderr: &syncBuffer{}, exit: make(chan int, 1), sendSignal: sendSignal}

	env = append([]string{envFakeUpstream + "=1"}, env...)
	go func() {
		code := cli.Run(runCtx, append([]string{"sprut", "serve", "-c", configPath}, args...), env,
			agentToGateway, io.MultiWriter(gatewayToAgent, g.stdout), g.stderr)
		_ = gatewayToAgent.Close()
		g.exit <- code
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: gatewayStdout, Writer: gatewayStdin}, nil)
	if err != nil {
		t.Fatalf("Agent failed to connect: %v\nstderr:\n%s", err, g.stderr)
	}
	g.Agent = session
	t.Cleanup(func() { _ = session.Close() })
	return g
}

// runSprut runs sprut to completion with args (without the program name)
// and env, and returns its exit code, stdout and stderr.
func runSprut(t *testing.T, env []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	code = cli.Run(ctx, append([]string{"sprut"}, args...), env, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

// closeAgent closes the Agent's side (EOF on the Gateway's stdin) and checks
// that the Gateway exits cleanly.
func (g *gateway) closeAgent(t *testing.T) {
	t.Helper()
	_ = g.Agent.Close()
	g.wantCleanExit(t, "EOF on stdin")
}

// wantCleanExit checks that the Gateway exits with code 0 after cause.
func (g *gateway) wantCleanExit(t *testing.T, cause string) {
	t.Helper()
	select {
	case code := <-g.exit:
		if code != 0 {
			t.Errorf("exit code = %d, want 0\nstderr:\n%s", code, g.stderr)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Gateway did not exit after %s\nstderr:\n%s", cause, g.stderr)
	}
}

// wantGone checks that the process whose PID is in pidFile (see
// envFakePIDFile and envFakeGrandchild) no longer exists.
func wantGone(t *testing.T, pidFile string) {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("PID not recorded: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d (%s) still exists after Gateway exit (kill -0: %v)", pid, pidFile, err)
	}
}

// waitForFile waits for the file at path to hold want.
func waitForFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %q (err %v), want %q", path, data, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startBlockingCall calls fake__block (see envFakeBlockTool, with progress
// as its path) under ctx, and returns once the Upstream is blocked in it. The
// call's error arrives on the returned channel when it ends.
func (g *gateway) startBlockingCall(t *testing.T, ctx context.Context, progress string) <-chan error {
	t.Helper()
	called := make(chan error, 1)
	go func() {
		_, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "fake__block"})
		called <- err
	}()
	waitForFile(t, progress, "started")
	return called
}

// resultText returns the one text a tool result holds.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content = %v, want one text", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %v, want text", res.Content[0])
	}
	return text.Text
}

// wantTools checks that the Agent's tools are exactly want, by name.
func (g *gateway) wantTools(t *testing.T, want ...string) {
	t.Helper()
	res, err := g.Agent.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v\nstderr:\n%s", got, want, g.stderr)
	}
}

// wantLogLine checks that some stderr line has level and contains every one
// of parts.
func (g *gateway) wantLogLine(t *testing.T, level string, parts ...string) {
	t.Helper()
	for _, line := range g.stderr.Lines() {
		if strings.Contains(line, "level="+level+" ") &&
			!slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
			return
		}
	}
	t.Errorf("no %s line containing all of %q; stderr:\n%s", level, parts, g.stderr)
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Lines() []string { return lines(b.String()) }

// lines splits s into lines without their newlines.
func lines(s string) []string {
	var out []string
	for line := range strings.Lines(s) {
		out = append(out, strings.TrimSuffix(line, "\n"))
	}
	return out
}

// toJSON normalises v through a JSON round trip, so that values decoded from
// the wire compare equal to the literals they were encoded from.
func toJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// callResult reduces a tool result to what the Upstream's tool produced: it
// drops the protocol's resultType and checks that the per-hop serverInfo in
// _meta names the Gateway, not the Upstream, before dropping it too.
func callResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	m := toJSON(t, res).(map[string]any)
	delete(m, "resultType")
	if meta, ok := m["_meta"].(map[string]any); ok {
		if info, ok := meta[mcp.MetaKeyServerInfo].(map[string]any); ok {
			if info["name"] != "sprut" {
				t.Errorf("result _meta serverInfo = %v, want the Gateway (sprut)", info)
			}
			delete(meta, mcp.MetaKeyServerInfo)
		}
		if len(meta) == 0 {
			delete(m, "_meta")
		}
	}
	return m
}
