package cli_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// execute calls the execute tool with code and returns its result.
func (g *gateway) execute(ctx context.Context, t *testing.T, code string) *mcp.CallToolResult {
	t.Helper()
	res, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: map[string]any{"code": code}})
	if err != nil {
		t.Fatalf("calling execute: %v", err)
	}
	return res
}

func TestServeOffersExecuteBesideNamespacedToolsOnlyInCodeMode(t *testing.T) {
	t.Parallel()
	startFakeGateway(t).wantTools(t, "fake__echo", "fake__fail")
	startFakeGateway(t, "--code-mode").wantTools(t, "execute", "fake__echo", "fake__fail")
}

func TestExecuteIsReadOnlyToolTakingCode(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t, "--code-mode")
	res, err := g.Agent.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var execute *mcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "execute" {
			execute = tool
		}
	}
	if execute == nil {
		t.Fatal("no execute tool")
	}

	wantSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"code": map[string]any{"type": "string", "description": "the script: the body of an async function"},
		},
		"required": []any{"code"},
	}
	if diff := cmp.Diff(toJSON(t, wantSchema), toJSON(t, execute.InputSchema)); diff != "" {
		t.Errorf("input schema (-want +got):\n%s", diff)
	}
	wantAnnotations := map[string]any{"readOnlyHint": true, "openWorldHint": true, "idempotentHint": false}
	if diff := cmp.Diff(toJSON(t, wantAnnotations), toJSON(t, execute.Annotations)); diff != "" {
		t.Errorf("annotations (-want +got):\n%s", diff)
	}
	for _, part := range []string{"async function", "await", "console.log", "return value", "JSON", "TypeScript", "50s", "64 MB", "40000 characters", "call depth"} {
		if !strings.Contains(execute.Description, part) {
			t.Errorf("description lacks %q:\n%s", part, execute.Description)
		}
	}
}

func TestExecuteReturnsWhatScriptPrintsThenItsReturnValue(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t, "--code-mode")
	ctx := t.Context()

	tests := []struct {
		code string
		want string
	}{
		{
			code: `console.log("sum", 1 + 2); console.error({ok: true}); console.warn([1]); return await Promise.resolve({n: 1})`,
			want: "sum 3\n" + `{"ok":true}` + "\n[1]\n" + `{"n":1}` + "\n",
		},
		{code: `console.log("nothing returned")`, want: "nothing returned\n"},
		{code: `return "text"`, want: `"text"` + "\n"},
	}
	for _, tt := range tests {
		res := g.execute(ctx, t, tt.code)
		if res.IsError {
			t.Errorf("%s: isError result %q", tt.code, resultText(t, res))
			continue
		}
		if got := resultText(t, res); got != tt.want {
			t.Errorf("%s: result %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestExecuteGivesIsErrorWithLineThenOutputSoFar(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t, "--code-mode")
	ctx := t.Context()

	tests := []struct {
		code string
		want string
	}{
		{code: "const x: number = 1", want: "SyntaxError: missing initializer for const variable (line 1)"},
		{code: "console.log('step 1')\nnull.x", want: "TypeError: cannot read property 'x' of null (line 2)\nstep 1\n"},
		{code: "console.log('step 1')\nawait 0\n\nthrow new Error('boom')", want: "Error: boom (line 4)\nstep 1\n"},
	}
	for _, tt := range tests {
		res := g.execute(ctx, t, tt.code)
		if !res.IsError {
			t.Errorf("%q: result %q is not isError", tt.code, resultText(t, res))
			continue
		}
		if got := resultText(t, res); got != tt.want {
			t.Errorf("%q: result %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestExecuteRejectsCallWithoutCode(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t, "--code-mode")
	res, err := g.Agent.CallTool(t.Context(), &mcp.CallToolParams{Name: "execute", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("calling execute: %v", err)
	}
	if !res.IsError || !strings.Contains(resultText(t, res), "code") {
		t.Errorf("result = %v, want an isError result asking for code", toJSON(t, res))
	}
}

func TestExecuteLoggedOnceUnderVerboseWithDurationAndOutcome(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t, "--code-mode", "-v")
	g.execute(t.Context(), t, "return 1")
	g.execute(t.Context(), t, "throw new Error('boom')")
	g.closeAgent(t)

	if n := g.loggedCount("DEBUG", `msg="script executed"`); n != 2 {
		t.Errorf("logged %d script executions, want 2", n)
	}
	g.wantLogLine(t, "DEBUG", `msg="script executed"`, "tool=execute", "duration=", "isError=false")
	g.wantLogLine(t, "DEBUG", `msg="script executed"`, "tool=execute", "duration=", "isError=true")
}

func TestServeStopsScriptWhenAgentCancelsExecute(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t, "--code-mode", "-v")

	ctx, cancel := context.WithCancel(t.Context())
	called := make(chan error, 1)
	go func() {
		_, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: map[string]any{"code": "for (;;) {}"}})
		called <- err
	}()
	// Give the call time to reach the Script before cancelling it. A cancel
	// that arrives first still ends the call with the context's error, but
	// then leaves stopping a running Script to the sandbox package's test.
	time.Sleep(200 * time.Millisecond)
	cancel() // the Agent sends notifications/cancelled

	if err := <-called; err == nil {
		t.Error("cancelled execute returned no error")
	}
	// A Script left running would end only at the 50s time limit, with an
	// isError result instead.
	if !eventually(func() bool { return g.logged("DEBUG", `msg="script executed"`, `err="context canceled"`) }) {
		t.Error(`no "script executed" line with the context's error`)
	}
	g.closeAgent(t)
}

func TestExecuteCutsOutputAt40000Characters(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t, "--code-mode")
	res := g.execute(t.Context(), t, `console.log("x".repeat(50000))`)
	want := strings.Repeat("x", 40000) + "\n[... 10001 more characters cut]\n"
	if got := resultText(t, res); res.IsError || got != want {
		t.Errorf("result (isError %v) of %d characters ending %q, want 40000 characters and a closing line", res.IsError, len(got), got[max(0, len(got)-40):])
	}
}

func TestExecuteGivesIsErrorWhenScriptTimesOutThenOutputSoFar(t *testing.T) {
	t.Parallel()
	g := startFakeGateway(t, "--code-mode", "--script-time-limit", "300ms")
	res := g.execute(t.Context(), t, "console.log('looping')\nfor (;;) {}")
	if got, want := resultText(t, res), "script timed out after 300ms\nlooping\n"; !res.IsError || got != want {
		t.Errorf("result (isError %v) %q, want isError %q", res.IsError, got, want)
	}
}
