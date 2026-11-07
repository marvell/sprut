package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/mcptest"
)

// startAuditedGateway runs `sprut serve --audit` plus args, with upstream as
// its one Upstream, fake, and a fresh state directory, which it returns.
func startAuditedGateway(t *testing.T, upstream mcptest.Stdio, args ...string) (*gateway, string) {
	t.Helper()
	state := t.TempDir()
	config := writeConfig(t, map[string]any{"fake": upstream.Entry(t)})
	return startGateway(t, config, []string{"XDG_STATE_HOME=" + state}, append([]string{"--audit"}, args...)...), state
}

// auditEvents reads the events of the one Audit log file under state, of
// the test Agent, dropping each event's ts.
func auditEvents(t *testing.T, state string) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(state, "sprut", "audit", "test-agent", "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("Audit log files = %v (%v), want one", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("line %q: %v", scanner.Text(), err)
		}
		delete(ev, "ts")
		events = append(events, ev)
	}
	return events
}

// withoutMS drops ms from events, whose value no test controls, after
// checking it is there.
func withoutMS(t *testing.T, events []map[string]any) []map[string]any {
	t.Helper()
	for _, ev := range events {
		if _, ok := ev["ms"].(float64); !ok && ev["type"] != "session" {
			t.Errorf("event %v has no ms", ev)
		}
		delete(ev, "ms")
	}
	return events
}

// sentBytes is the size of the JSON of res, as the Gateway sent it: without
// the resultType and _meta serverInfo that the SDK adds on the way out.
func sentBytes(t *testing.T, res *mcp.CallToolResult) float64 {
	t.Helper()
	data, err := json.Marshal(callResult(t, res))
	if err != nil {
		t.Fatal(err)
	}
	return float64(len(data))
}

func TestServeWritesNoAuditLogWithoutAudit(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	g := startGateway(t, writeConfig(t, map[string]any{"fake": mcptest.Stdio{}.Entry(t)}), []string{"XDG_STATE_HOME=" + state}, "--code-mode")
	g.wantEcho(t)
	g.wantExecuted(t, `return 1`, "1\n")
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Errorf("state directory holds %v, want nothing", entries)
	}
}

func TestAuditLogStartsWithSessionOfAgentInEitherEra(t *testing.T) {
	t.Parallel()
	for _, protocol := range []string{mcptest.Legacy, mcptest.Modern} {
		t.Run(protocol, func(t *testing.T) {
			t.Parallel()
			state := t.TempDir()
			config := writeConfig(t, map[string]any{"fake": mcptest.Stdio{}.Entry(t)})
			g := startGatewayAs(t, protocol, config, []string{"XDG_STATE_HOME=" + state}, "--audit")
			g.wantEcho(t) // a modern Agent makes its first request only now

			events := auditEvents(t, state)
			session := events[0]
			if v, _ := session["sprut"].(string); v == "" {
				t.Errorf("session has no sprut version: %v", session)
			}
			delete(session, "sprut")
			want := map[string]any{"type": "session", "agent": "test-agent", "agent_version": "0.0.1", "protocol": protocol, "code_mode": false}
			if diff := cmp.Diff(want, session); diff != "" {
				t.Errorf("session event (-want +got):\n%s", diff)
			}
			if got := len(events); got != 2 {
				t.Errorf("%d events, want the session's and the call's: %v", got, events)
			}
		})
	}
}

func TestAuditLogIsReadableOnlyByItsOwner(t *testing.T) {
	t.Parallel()
	g, state := startAuditedGateway(t, mcptest.Stdio{})
	g.wantEcho(t)

	paths, err := filepath.Glob(filepath.Join(state, "sprut", "audit", "test-agent", "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("Audit log files = %v (%v), want one", paths, err)
	}
	for _, p := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(state, "sprut"), 0o700},
		{filepath.Join(state, "sprut", "audit"), 0o700},
		{filepath.Join(state, "sprut", "audit", "test-agent"), 0o700},
		{paths[0], 0o600},
	} {
		fi, err := os.Stat(p.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != p.mode {
			t.Errorf("%s has mode %o, want %o", p.path, got, p.mode)
		}
	}
}

func TestAuditLogRecordsDirectCallsWithOutcomeAndSizes(t *testing.T) {
	t.Parallel()
	g, state := startAuditedGateway(t, mcptest.Stdio{})
	ctx := t.Context()
	echo := g.echo(ctx, t)
	fail, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "fake__fail", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}

	want := []map[string]any{
		{"type": "call", "tool": "fake__echo", "upstream": "fake", "script": nil, "outcome": "ok", "args_bytes": float64(len(`{"text":"hi"}`)), "result_bytes": sentBytes(t, echo)},
		{"type": "call", "tool": "fake__fail", "upstream": "fake", "script": nil, "outcome": "error", "error": "it failed", "args_bytes": 2.0, "result_bytes": sentBytes(t, fail)},
	}
	if diff := cmp.Diff(want, withoutMS(t, auditEvents(t, state)[1:])); diff != "" {
		t.Errorf("call events (-want +got):\n%s", diff)
	}
}

func TestAuditLogRecordsScriptsWithTheirCallsByNumber(t *testing.T) {
	t.Parallel()
	g, state := startAuditedGateway(t, mcptest.Stdio{WriteTool: true}, "--code-mode")
	ctx := t.Context()
	direct := g.echo(ctx, t) // what a direct call of the Script's first call sends
	code := `const a = await call("fake__echo", {text: "hi"});
let refused = "";
try { await call("fake__write", {}) } catch (e) { refused = "refused" }
const b = await call("fake__echo", {text: "there"});
return [a.echoed, b.echoed, refused]`
	first := g.execute(ctx, t, code)
	failed := g.execute(ctx, t, `throw new Error("boom")`)

	events := withoutMS(t, auditEvents(t, state))[2:] // after the session's and the direct call's
	echoThere := events[1]["result_bytes"]
	want := []map[string]any{
		{"type": "call", "tool": "fake__echo", "upstream": "fake", "script": 1.0, "outcome": "ok", "args_bytes": float64(len(`{"text":"hi"}`)), "result_bytes": sentBytes(t, direct)},
		{"type": "call", "tool": "fake__echo", "upstream": "fake", "script": 1.0, "outcome": "ok", "args_bytes": float64(len(`{"text":"there"}`)), "result_bytes": echoThere},
		{"type": "script", "script": 1.0, "outcome": "ok", "calls": 2.0, "code": code, "code_bytes": float64(len(code)), "result_bytes": sentBytes(t, first)},
		{"type": "script", "script": 2.0, "outcome": "error", "error": resultText(t, failed), "calls": 0.0, "code": `throw new Error("boom")`, "code_bytes": 23.0, "result_bytes": sentBytes(t, failed)},
	}
	if diff := cmp.Diff(want, events); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	if n, _ := echoThere.(float64); n <= sentBytes(t, direct) {
		t.Errorf("echo of %q took %v bytes, want more than echo of %q (%v)", "there", echoThere, "hi", sentBytes(t, direct))
	}
}

func TestAuditLogRecordsCancelledCall(t *testing.T) {
	t.Parallel()
	progress := filepath.Join(t.TempDir(), "block")
	g, state := startAuditedGateway(t, mcptest.Stdio{BlockToolFile: progress})
	ctx, cancel := context.WithCancel(t.Context())
	called := g.startBlockingCall(ctx, t, progress)
	cancel()
	<-called

	var events []map[string]any
	if !eventually(func() bool { events = auditEvents(t, state); return len(events) == 2 }) {
		t.Fatalf("events = %v, want the session's and the call's", events)
	}
	if got := events[1]["outcome"]; got != "cancelled" {
		t.Errorf("outcome = %v, want cancelled: %v", got, events[1])
	}
}

func TestAuditLogThatCannotBeWrittenIsLoggedOnceAndServingGoesOn(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(state, nil, 0o600); err != nil { // a file, so nothing can be made under it
		t.Fatal(err)
	}
	config := writeConfig(t, map[string]any{"fake": mcptest.Stdio{}.Entry(t)})
	g := startGateway(t, config, []string{"XDG_STATE_HOME=" + state}, "--audit")
	g.wantEcho(t)
	g.wantEcho(t)
	if got := g.loggedCount("ERROR", `msg="audit log disabled"`); got != 1 {
		t.Errorf("logged %d ERROR lines saying the audit log is disabled, want 1:\n%s", got, strings.Join(g.stderr.Lines(), "\n"))
	}
	g.closeAgent(t)
}
