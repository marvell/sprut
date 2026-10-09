package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/cli"
	"github.com/marvell/sprut/internal/mcptest"
)

func TestMain(m *testing.M) {
	mcptest.MainIfFake()
	os.Exit(m.Run())
}

// startFakeGateway runs `sprut serve` with a Config holding one well-behaved
// fake Upstream, "fake".
func startFakeGateway(t *testing.T) *gateway {
	t.Helper()
	return startGateway(t, writeConfig(t, map[string]any{"fake": mcptest.Stdio{}.Entry(t)}), nil)
}

// writeConfig writes a Config with the given Upstreams and returns its path.
func writeConfig(t *testing.T, upstreams map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigAt(t, path, upstreams)
	return path
}

// writeConfigAt writes a Config with the given Upstreams at path, making
// its directory.
func writeConfigAt(t *testing.T, path string, upstreams map[string]any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"mcpServers": upstreams})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
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
	return startGatewayAs(t, "", configPath, env, args...)
}

// startGatewayAs is startGateway with an Agent that asks for protocol, which
// sets its era. An empty protocol is the SDK's latest.
func startGatewayAs(t *testing.T, protocol, configPath string, env []string, args ...string) *gateway {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	// main delivers signals by cancelling cli.Run's ctx.
	runCtx, sendSignal := context.WithCancel(ctx)

	agentToGateway, gatewayStdin := io.Pipe()
	gatewayStdout, gatewayToAgent := io.Pipe()
	g := &gateway{stdout: &syncBuffer{}, stderr: &syncBuffer{}, exit: make(chan int, 1), sendSignal: sendSignal}

	go func() {
		code := cli.Run(runCtx, append([]string{"sprut", "serve", "-c", configPath}, args...), env,
			agentToGateway, io.MultiWriter(gatewayToAgent, g.stdout), g.stderr)
		_ = gatewayToAgent.Close()
		g.exit <- code
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: gatewayStdout, Writer: gatewayStdin},
		&mcp.ClientSessionOptions{ProtocolVersion: protocol})
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
	return runSprutOn(t, strings.NewReader(""), env, args...)
}

// runSprutOn is runSprut with stdin read from stdin.
func runSprutOn(t *testing.T, stdin io.Reader, env []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	code = cli.Run(ctx, append([]string{"sprut"}, args...), env, stdin, &out, &errOut)
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

// wantGone checks that the process whose PID is in pidFile (one of the
// ...PIDFile fields of mcptest.Stdio) no longer exists.
func wantGone(t *testing.T, pidFile string) {
	t.Helper()
	pid := readPID(t, pidFile)
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d (%s) still exists after Gateway exit (kill -0: %v)", pid, pidFile, err)
	}
}

// waitForExit waits for the process whose PID is in pidFile to no longer
// exist, while the Gateway keeps running.
func waitForExit(t *testing.T, pidFile string) {
	t.Helper()
	pid := readPID(t, pidFile)
	var err error
	if !eventually(func() bool { err = syscall.Kill(pid, 0); return errors.Is(err, syscall.ESRCH) }) {
		t.Fatalf("process %d (%s) still exists (kill -0: %v)", pid, pidFile, err)
	}
}

// readPID reads the PID recorded in pidFile.
func readPID(t *testing.T, pidFile string) int {
	t.Helper()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("PID not recorded: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitForFile waits for the file at path to hold want.
func waitForFile(t *testing.T, path, want string) {
	t.Helper()
	var data []byte
	var err error
	if !eventually(func() bool { data, err = os.ReadFile(path); return err == nil && string(data) == want }) {
		t.Fatalf("%s holds %q (err %v), want %q", path, data, err, want)
	}
}

// eventually polls cond until it holds, for up to 10s, and reports whether it
// did.
func eventually(cond func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// startBlockingCall calls fake__block (see mcptest.Stdio.BlockToolFile,
// with progress as its path) under ctx, and returns once the Upstream is blocked in it. The
// call's error arrives on the returned channel when it ends.
func (g *gateway) startBlockingCall(ctx context.Context, t *testing.T, progress string) <-chan error {
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
	if !g.logged(level, parts...) {
		t.Errorf("no %s line containing all of %q; stderr:\n%s", level, parts, g.stderr)
	}
}

// wantNoLogLine checks that the Gateway has logged no line at level
// containing all of parts.
func (g *gateway) wantNoLogLine(t *testing.T, level string, parts ...string) {
	t.Helper()
	if g.logged(level, parts...) {
		t.Errorf("a %s line contains all of %q; stderr:\n%s", level, parts, g.stderr)
	}
}

// logged reports whether the Gateway has logged a line at level containing
// all of parts.
func (g *gateway) logged(level string, parts ...string) bool {
	return slices.ContainsFunc(g.stderr.Lines(), func(line string) bool {
		return strings.Contains(line, "level="+level+" ") &&
			!slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) })
	})
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
