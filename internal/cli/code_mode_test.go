package cli_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/mcptest"
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
	execute := startFakeGateway(t, "--code-mode").executeTool(t)

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
	if first, _, _ := strings.Cut(execute.Description, "\n"); !strings.Contains(first, "read-only tools") || !strings.Contains(first, "context") {
		t.Errorf("description starts %q, want it to lead with composing read-only tools outside the Agent's context", first)
	}
	for _, part := range []string{`await call("<upstream>__<tool>", args)`, "readOnlyHint", "call it directly", "structuredContent", "isError", "async function", "await", "console.log", "return value", "JSON", "TypeScript", "50s", "64 MB", "40000 characters", "call depth"} {
		if !strings.Contains(execute.Description, part) {
			t.Errorf("description lacks %q:\n%s", part, execute.Description)
		}
	}
}

// executeTool is the execute tool as the Agent lists it.
func (g *gateway) executeTool(t *testing.T) *mcp.Tool {
	t.Helper()
	tools := g.tools(t)
	i := slices.IndexFunc(tools, func(tool *mcp.Tool) bool { return tool.Name == "execute" })
	if i < 0 {
		t.Fatal("no execute tool")
	}
	return tools[i]
}

func TestExecuteDescriptionListsReadOnlyToolsByUpstreamAfterScriptContract(t *testing.T) {
	t.Parallel()
	upstreams := map[string]any{
		"linear": mcptest.Stdio{ReplyTool: true, BlockToolFile: filepath.Join(t.TempDir(), "block")}.Entry(t),
		"fake":   mcptest.Stdio{}.Entry(t),
	}
	description := startGateway(t, writeConfig(t, upstreams), nil, "--code-mode").executeTool(t).Description

	want := "\n\nTools a script can call, by upstream, named exactly as in your tool list, where their input schemas are:\n" +
		"- fake: fake__echo\n" +
		"- linear: linear__block, linear__echo, linear__reply\n"
	if !strings.HasSuffix(description, want) {
		t.Errorf("description doesn't end with the tool list %q:\n%s", want, description)
	}
	if contract, _, _ := strings.Cut(description, "Tools a script can call"); !strings.Contains(contract, "Limits:") {
		t.Errorf("tool list comes before the Script contract:\n%s", description)
	}
}

func TestExecuteDescriptionLeavesOutToolsThatAreNotReadOnly(t *testing.T) {
	t.Parallel()
	description := startCodeModeGateway(t, mcptest.Stdio{WriteTool: true, ExtraTools: []string{"plain"}}).executeTool(t).Description

	for _, tool := range []string{"fake__write", "fake__fail", "fake__plain"} { // marked as writing, and not marked
		if strings.Contains(description, tool) {
			t.Errorf("description lists %s:\n%s", tool, description)
		}
	}
	if !strings.HasSuffix(description, "\n- fake: fake__echo\n") {
		t.Errorf("description doesn't end listing fake__echo alone:\n%s", description)
	}
}

func TestExecuteServedSayingNoToolIsCallableWhileNoneIsReadOnly(t *testing.T) {
	t.Parallel()
	g := startGateway(t, writeConfig(t, map[string]any{}), nil, "--code-mode")
	description := g.executeTool(t).Description
	want := "\n\nNo tool can be called from a script: no upstream marks one readOnlyHint: true.\n"
	if !strings.HasSuffix(description, want) {
		t.Errorf("description doesn't end with %q:\n%s", want, description)
	}
	g.wantExecuted(t, "return 1", "1\n")
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

func TestServeStopsScriptAndItsCallWhenAgentCancelsExecute(t *testing.T) {
	t.Parallel()
	progress := filepath.Join(t.TempDir(), "block")
	g := startCodeModeGateway(t, mcptest.Stdio{BlockToolFile: progress}, "-v")

	ctx, cancel := context.WithCancel(t.Context())
	called := make(chan error, 1)
	go func() {
		_, err := g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: map[string]any{"code": `await call("fake__block")`}})
		called <- err
	}()
	waitForFile(t, progress, "started")
	cancel() // the Agent sends notifications/cancelled

	if err := <-called; err == nil {
		t.Error("cancelled execute returned no error")
	}
	waitForFile(t, progress, "cancelled")
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
	g := startFakeGateway(t, "--code-mode", "--script-time-limit", "1s")
	res := g.execute(t.Context(), t, "console.log('looping')\nfor (;;) {}")
	if got, want := resultText(t, res), "script timed out after 1s\nlooping\n"; !res.IsError || got != want {
		t.Errorf("result (isError %v) %q, want isError %q", res.IsError, got, want)
	}
}

// startCodeModeGateway starts a Gateway in Code mode, with args, whose one
// Upstream, fake, is upstream.
func startCodeModeGateway(t *testing.T, upstream mcptest.Stdio, args ...string) *gateway {
	t.Helper()
	upstreams := map[string]any{"fake": upstream.Entry(t)}
	return startGateway(t, writeConfig(t, upstreams), nil, append([]string{"--code-mode"}, args...)...)
}

// startCallableGateway starts a Gateway in Code mode, with -v, whose one
// Upstream, fake, also offers reply and write.
func startCallableGateway(t *testing.T) *gateway {
	t.Helper()
	return startCodeModeGateway(t, mcptest.Stdio{ReplyTool: true, WriteTool: true}, "-v")
}

// wantExecuted checks that execute ran code and gave want, not an isError
// result.
func (g *gateway) wantExecuted(t *testing.T, code, want string) {
	t.Helper()
	res := g.execute(t.Context(), t, code)
	if got := resultText(t, res); res.IsError || got != want {
		t.Errorf("%s: result (isError %v) %q, want %q", code, res.IsError, got, want)
	}
}

// wantScriptError checks that execute ran code and gave an isError result
// containing every one of parts.
func (g *gateway) wantScriptError(t *testing.T, code string, parts ...string) {
	t.Helper()
	res := g.execute(t.Context(), t, code)
	got := resultText(t, res)
	if !res.IsError {
		t.Errorf("%s: result %q is not isError", code, got)
	}
	for _, part := range parts {
		if !strings.Contains(got, part) {
			t.Errorf("%s: result %q lacks %q", code, got, part)
		}
	}
}

func TestScriptCallReturnsStructuredContentFirst(t *testing.T) {
	t.Parallel()
	g := startCallableGateway(t)
	g.wantExecuted(t, `const r = await call("fake__echo", {text: "hi"}); return r.echoed`, `"hi"`+"\n")
	g.wantExecuted(t, `return await call("fake__reply", {content: [{type: "text", text: "[1]"}], structuredContent: {n: 2}})`, `{"n":2}`+"\n")
}

func TestScriptCallReturnsJSONOfItsOnlyTextBlock(t *testing.T) {
	t.Parallel()
	g := startCallableGateway(t)
	g.wantExecuted(t, `const r = await call("fake__reply", {content: [{type: "text", text: '{"ids": [1, 2]}'}]}); return r.ids.length`, "2\n")
}

func TestScriptCallReturnsTextOfItsOnlyTextBlockThatIsNotJSON(t *testing.T) {
	t.Parallel()
	g := startCallableGateway(t)
	g.wantExecuted(t, `const r = await call("fake__reply", {content: [{type: "text", text: "plain {"}]}); return typeof r + ": " + r`, `"string: plain {"`+"\n")
}

func TestScriptCallReturnsContentArrayOtherwise(t *testing.T) {
	t.Parallel()
	g := startCallableGateway(t)
	twoTexts := `{content: [{type: "text", text: "a"}, {type: "text", text: "1"}]}`
	g.wantExecuted(t, `return await call("fake__reply", `+twoTexts+`)`, `[{"type":"text","text":"a"},{"type":"text","text":"1"}]`+"\n")
	image := `{content: [{type: "image", data: "AAAA", mimeType: "image/png"}]}`
	g.wantExecuted(t, `return await call("fake__reply", `+image+`)`, `[{"type":"image","mimeType":"image/png","data":"AAAA"}]`+"\n")
	g.wantExecuted(t, `return await call("fake__reply", {content: []})`, "[]\n")
}

func TestScriptCallThrowsCatchableErrorOnIsErrorResult(t *testing.T) {
	t.Parallel()
	g := startCallableGateway(t)
	failing := `{isError: true, content: [{type: "text", text: "no such issue"}], structuredContent: {n: 1}}`
	g.wantExecuted(t, `try { await call("fake__reply", `+failing+`) } catch (e) { return e instanceof Error ? e.message : "not an Error" }`, `"no such issue"`+"\n")
	g.wantScriptError(t, "console.log('before')\nawait call(\"fake__reply\", "+failing+")", "Error: no such issue (line 2)\nbefore\n")
	g.wantScriptError(t, `await call("fake__reply", {isError: true, content: []})`, `Error: tool "fake__reply" returned an error (line 1)`)
}

func TestScriptCallRefusesToolThatIsNotReadOnly(t *testing.T) {
	t.Parallel()
	g := startCallableGateway(t)
	for _, tool := range []string{"fake__write", "fake__fail"} { // marked as writing, and not marked
		g.wantScriptError(t, `await call("`+tool+`")`, `"`+tool+`"`, "not read-only", "call it directly", "(line 1)")
	}
	g.closeAgent(t)
	g.wantNoLogLine(t, "DEBUG", `msg="tool called"`)
}

func TestScriptCallRefusesUnknownTool(t *testing.T) {
	t.Parallel()
	g := startCallableGateway(t)
	for _, tool := range []string{"fake__nope", "nope", "execute"} {
		g.wantScriptError(t, `await call("`+tool+`")`, `unknown tool "`+tool+`"`, "directly")
	}
	g.wantExecuted(t, `try { await call("fake__write") } catch (e) { return "caught" }`, `"caught"`+"\n")
}

func TestScriptCallThrowsWhenUpstreamAsksForInput(t *testing.T) {
	t.Parallel()
	g := startCodeModeGateway(t, mcptest.Stdio{Protocol: mcptest.Modern, AskTool: true})
	direct, err := g.Agent.CallTool(t.Context(), &mcp.CallToolParams{Name: "fake__ask"})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	g.wantScriptError(t, `await call("fake__ask")`, "Error: "+resultText(t, direct)+" (line 1)")
}

func TestScriptCallThrowsWhenCallToUpstreamFails(t *testing.T) {
	t.Parallel()
	upstreams := map[string]any{"doomed": mcptest.Stdio{CrashTool: true}.Entry(t)}
	g := startGateway(t, writeConfig(t, upstreams), nil, "--code-mode")
	g.wantScriptError(t, `await call("doomed__crash")`, `Error: call to upstream "doomed" failed`)
	direct, err := g.Agent.CallTool(t.Context(), &mcp.CallToolParams{Name: "doomed__echo", Arguments: map[string]any{"text": "x"}})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	g.wantScriptError(t, `await call("doomed__echo", {text: "x"})`, "Error: "+resultText(t, direct)+" (line 1)")
}

func TestScriptCallsLoggedUnderVerboseLikeDirectCallsMarkedAsFromExecute(t *testing.T) {
	t.Parallel()
	g := startCallableGateway(t)
	g.wantExecuted(t, `await call("fake__echo", {text: "a"}); await call("fake__reply", {isError: true, content: []}).catch(() => {})`, "")
	g.closeAgent(t)

	g.wantLogLine(t, "DEBUG", `msg="tool called"`, "upstream=fake", "tool=fake__echo", "via=execute", "duration=", "isError=false")
	g.wantLogLine(t, "DEBUG", `msg="tool called"`, "upstream=fake", "tool=fake__reply", "via=execute", "isError=true")
}

// gatherAll is a Script that calls fake__gather n times at once.
func gatherAll(n int) string {
	return fmt.Sprintf(`const r = await Promise.all(Array.from({length: %d}, () => call("fake__gather"))); return r.length`, n)
}

func TestScriptCallsStartedTogetherRunConcurrentlyEightAtATime(t *testing.T) {
	t.Parallel()
	// The Upstream answers only once eight calls are in flight to it; the
	// eight after them wait their turn and then are answered at once.
	g := startCodeModeGateway(t, mcptest.Stdio{GatherTool: 8})
	g.wantExecuted(t, gatherAll(16), "16\n")
}

func TestScriptCallsBeyondEightWaitWhileEightAreInFlight(t *testing.T) {
	t.Parallel()
	// Nine calls in flight at once would be answered; eight at a time never
	// are, so the Script runs out of time.
	g := startCodeModeGateway(t, mcptest.Stdio{GatherTool: 9}, "--script-time-limit", "1s")
	g.wantScriptError(t, gatherAll(9), "script timed out after 1s")
}

func TestScriptMayMakeAtMost100Calls(t *testing.T) {
	t.Parallel()
	g := startCallableGateway(t)
	const reply = `call("fake__reply", {content: [{type: "text", text: "1"}]})`
	g.wantExecuted(t, `let n = 0
try {
	for (;;) n += await `+reply+`
} catch (e) {
	return [n, e.message]
}`, `[100,"a script may make at most 100 calls"]`+"\n")
	g.wantScriptError(t, `console.log("looping")
for (;;) await `+reply,
		"a script may make at most 100 calls (line 2)\nlooping\n")
}

func TestScriptRunOutOfTimeCancelsItsCallsInFlight(t *testing.T) {
	t.Parallel()
	progress := filepath.Join(t.TempDir(), "block")
	g := startCodeModeGateway(t, mcptest.Stdio{BlockToolFile: progress}, "--script-time-limit", "2s")

	g.wantScriptError(t, `await call("fake__block")`, "script timed out after 2s")
	waitForFile(t, progress, "cancelled")
	g.closeAgent(t)
}

func TestExecuteDescriptionStatesConcurrencyAndCallCaps(t *testing.T) {
	t.Parallel()
	description := startFakeGateway(t, "--code-mode").executeTool(t).Description
	for _, want := range []string{
		"Up to 8 calls run at once, so Promise.all over calls runs them in parallel; the others wait their turn.",
		"A script may make at most 100 calls: the next one throws an Error.",
	} {
		if !strings.Contains(description, want) {
			t.Errorf("description lacks %q:\n%s", want, description)
		}
	}
	if strings.Contains(description, "one at a time") {
		t.Errorf("description still says calls run one at a time:\n%s", description)
	}
}
