package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
)

func TestMain(m *testing.M) {
	if os.Getenv(envFakeUpstream) != "" {
		os.Exit(runFakeUpstream())
	}
	os.Exit(m.Run())
}

// The fake Upstream's tools, as raw JSON so that tests can assert that the
// Gateway passes them through unchanged.
var (
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
		InputSchema: map[string]any{"type": "object"},
	}
)

func runFakeUpstream() int {
	if path := os.Getenv(envFakePIDFile); path != "" {
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "fake upstream:", err)
			return 1
		}
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
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "fake upstream:", err)
		return 1
	}
	return 0
}

// startFakeGateway runs `sprut serve` with a Config holding one Upstream,
// "fake", launched with env added to its environment.
func startFakeGateway(t *testing.T, env ...string) *gateway {
	t.Helper()
	return startGateway(t, writeConfig(t, map[string]any{"fake": fakeUpstreamEntry(t)}), env)
}

// fakeUpstreamEntry returns a Config entry that launches the fake Upstream.
func fakeUpstreamEntry(t *testing.T) map[string]any {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"command": exe, "args": []string{"-test.run=^$"}}
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
	Agent  *mcp.ClientSession
	stdout *syncBuffer // everything the Gateway wrote to stdout
	stderr *syncBuffer
	exit   chan int
}

// startGateway runs `sprut serve -c configPath` with env and connects an
// Agent to it.
func startGateway(t *testing.T, configPath string, env []string) *gateway {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	agentToGateway, gatewayStdin := io.Pipe()
	gatewayStdout, gatewayToAgent := io.Pipe()
	g := &gateway{stdout: &syncBuffer{}, stderr: &syncBuffer{}, exit: make(chan int, 1)}

	env = append([]string{envFakeUpstream + "=1"}, env...)
	go func() {
		code := cli.Run(ctx, []string{"sprut", "serve", "-c", configPath}, env,
			agentToGateway, io.MultiWriter(gatewayToAgent, g.stdout), g.stderr)
		gatewayToAgent.Close()
		g.exit <- code
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: gatewayStdout, Writer: gatewayStdin}, nil)
	if err != nil {
		t.Fatalf("Agent failed to connect: %v\nstderr:\n%s", err, g.stderr)
	}
	g.Agent = session
	t.Cleanup(func() { session.Close() })
	return g
}

// closeAgent closes the Agent's side (EOF on the Gateway's stdin) and returns
// the Gateway's exit code.
func (g *gateway) closeAgent(t *testing.T) int {
	t.Helper()
	g.Agent.Close()
	select {
	case code := <-g.exit:
		return code
	case <-time.After(15 * time.Second):
		t.Fatalf("Gateway did not exit after EOF on stdin\nstderr:\n%s", g.stderr)
		return -1
	}
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

func (b *syncBuffer) Lines() []string {
	var lines []string
	for line := range strings.Lines(b.String()) {
		lines = append(lines, strings.TrimSuffix(line, "\n"))
	}
	return lines
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
