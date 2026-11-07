package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marvell/sprut/internal/sandbox"
)

// noTools is a Caller that offers no tool.
func noTools(_ context.Context, name string, _ json.RawMessage) (json.RawMessage, error) {
	return nil, fmt.Errorf("no tool %q", name)
}

// run runs script with no limits and fails the test if it fails.
func run(t *testing.T, script string) string {
	t.Helper()
	out, err := sandbox.Run(t.Context(), script, noTools, sandbox.Limits{})
	if err != nil {
		t.Fatalf("Run(%q): %v", script, err)
	}
	return out
}

// runFailing runs script under limits and returns its result and the
// *sandbox.Error it must fail with.
func runFailing(t *testing.T, script string, limits sandbox.Limits) (string, *sandbox.Error) {
	t.Helper()
	out, err := sandbox.Run(t.Context(), script, noTools, limits)
	var scriptErr *sandbox.Error
	if !errors.As(err, &scriptErr) {
		t.Fatalf("Run(%q) = %q, %v; want a *sandbox.Error", script, out, err)
	}
	return out, scriptErr
}

func TestScriptSeesNoHostAPIBeyondConsole(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"fetch", "require", "std", "os", "print", "process", "module", "exports",
		"setTimeout", "setInterval", "setImmediate", "queueMicrotask",
		"XMLHttpRequest", "WebSocket", "Deno", "Bun", "__sprut_print", "__sprut_call",
	} {
		if got := run(t, "return typeof "+name); got != `"undefined"`+"\n" {
			t.Errorf("typeof %s = %s, want undefined", name, got)
		}
	}
	if got := run(t, "return typeof console.log"); got != `"function"`+"\n" {
		t.Errorf("typeof console.log = %s, want function", got)
	}
}

func TestScriptCannotLoadModules(t *testing.T) {
	t.Parallel()
	_, err := runFailing(t, `await import("os")`, sandbox.Limits{})
	if !strings.Contains(err.Message, "could not load module") {
		t.Errorf("error = %q, want it to say the module could not load", err.Message)
	}
	_, err = runFailing(t, `import * as os from "os"`, sandbox.Limits{})
	if !strings.Contains(err.Message, "SyntaxError") {
		t.Errorf("error = %q, want a SyntaxError for a static import", err.Message)
	}
}

func TestEachRunStartsFromFreshVM(t *testing.T) {
	t.Parallel()
	run(t, `globalThis.leaked = 1; Array.prototype.leaked = 2; console.log = () => {}`)
	got := run(t, `console.log(typeof leaked, typeof [].leaked)`)
	if got != "undefined undefined\n" {
		t.Errorf("second Script printed %q, want nothing left by the first", got)
	}
}

func TestConsoleFormatsLikeNode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		script string
		want   string
	}{
		{`console.log("plain", "text")`, "plain text\n"},
		{`console.log({a: 1, b: [true, null]}, 2)`, `{"a":1,"b":[true,null]} 2` + "\n"},
		{`console.log(undefined, 10n, new Error("boom"))`, "undefined 10 Error: boom\n"},
		{`console.log()`, "\n"},
		{`console.log("a"); console.error("b"); console.warn("c")`, "a\nb\nc\n"},
		{`const o = {}; o.self = o; console.log(o)`, "[object Object]\n"},
	}
	for _, tt := range tests {
		if got := run(t, tt.script); got != tt.want {
			t.Errorf("%s printed %q, want %q", tt.script, got, tt.want)
		}
	}
}

func TestScriptIsAsyncFunctionBodyWhoseReturnValueFollowsOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		script string
		want   string
	}{
		{`console.log("first"); return await Promise.resolve({n: 1})`, "first\n" + `{"n":1}` + "\n"},
		{`return "text"`, `"text"` + "\n"},
		{`console.log("only output")`, "only output\n"},
		{`return undefined`, ""},
		{`return null`, "null\n"},
	}
	for _, tt := range tests {
		if got := run(t, tt.script); got != tt.want {
			t.Errorf("%s gave %q, want %q", tt.script, got, tt.want)
		}
	}
}

func TestFailingScriptGivesErrorWithLineAndOutputSoFar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		script string
		want   string
	}{
		{"console.log('before')\nlet x = ;", "SyntaxError: unexpected token in expression: ';' (line 2)"},
		{"console.log('before')\n\nnull.x", "TypeError: cannot read property 'x' of null (line 3)"},
		{"console.log('before')\nawait 1\nthrow new Error('boom')", "Error: boom (line 3)"},
		{"console.log('before')\nawait Promise.reject(new RangeError('nope'))", "RangeError: nope (line 2)"},
		{"console.log('before')\nthrow 'str'", "uncaught str"},
		{"console.log('before')\nawait new Promise(() => {})", "the script awaited a promise that never settles"},
	}
	for _, tt := range tests {
		out, err := runFailing(t, tt.script, sandbox.Limits{})
		if err.Message != tt.want {
			t.Errorf("%q failed with %q, want %q", tt.script, err.Message, tt.want)
		}
		wantOut := tt.want + "\nbefore\n"
		if strings.HasPrefix(tt.want, "SyntaxError") {
			wantOut = tt.want // nothing runs
		}
		if out != wantOut {
			t.Errorf("%q gave %q, want %q", tt.script, out, wantOut)
		}
	}
}

func TestScriptStoppedAtTimeLimit(t *testing.T) {
	t.Parallel()
	for _, script := range []string{"for (;;) {}", "await 1; for (;;) {}"} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			out, err := runFailing(t, "console.log('looping')\n"+script, sandbox.Limits{Time: 100 * time.Millisecond})
			if err.Message != "script timed out after 100ms" || out != "script timed out after 100ms\nlooping\n" {
				t.Errorf("error %q with result %q, want a timeout, then its output", err.Message, out)
			}
		})
	}
}

func TestScriptFailsAtMemoryLimit(t *testing.T) {
	t.Parallel()
	_, err := runFailing(t, "const a = []; for (;;) a.push('x'.repeat(1024) + a.length)", sandbox.Limits{Memory: 16 << 20})
	if !strings.Contains(err.Message, "out of memory") {
		t.Errorf("error = %q, want out of memory", err.Message)
	}
}

func TestCancelledRunStopsScriptAndReturnsContextError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	out, err := sandbox.Run(ctx, "for (;;) {}", noTools, sandbox.Limits{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %q, %v; want context.Canceled", out, err)
	}
}

func TestDeepRecursionFailsAtStackLimit(t *testing.T) {
	t.Parallel()
	const recurse = "function depth(n) { return n === 0 ? 0 : 1 + depth(n - 1) }\n"
	limits := sandbox.Limits{Stack: 200}
	if out, err := sandbox.Run(t.Context(), recurse+"return depth(50)", noTools, limits); err != nil || out != "50\n" {
		t.Errorf("recursing 50 deep = %q, %v; want 50", out, err)
	}
	out, err := runFailing(t, recurse+"console.log('recursing')\nreturn depth(1000)", limits)
	if !strings.Contains(err.Message, "stack overflow") || out != err.Message+"\nrecursing\n" {
		t.Errorf("error %q with result %q, want a stack overflow, then its output", err.Message, out)
	}
}

func TestUnboundedRecursionFailsWithoutCrashing(t *testing.T) {
	t.Parallel()
	_, err := runFailing(t, "function f() { return f() + 1 }\nf()", sandbox.Limits{})
	if !strings.Contains(err.Message, "stack overflow") {
		t.Errorf("error = %q, want a stack overflow", err.Message)
	}
}

func TestOutputCutAtLimitWithClosingLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		script string
		want   string
	}{
		{`console.log("012345678")`, "012345678\n"},
		{`console.log("0123456789abcdef")`, "0123456789\n[... 7 more characters cut]\n"},
		{`console.log("abc"); return "x".repeat(20)`, "abc\n\"xxxxx\n[... 17 more characters cut]\n"},
		{`console.log("ж".repeat(12))`, "жжжжжжжжжж\n[... 3 more characters cut]\n"},
		{`for (let i = 0; i < 5; i++) console.log("0123456789")`, "0123456789\n[... 45 more characters cut]\n"},
	}
	for _, tt := range tests {
		out, err := sandbox.Run(t.Context(), tt.script, noTools, sandbox.Limits{Output: 10})
		if err != nil || out != tt.want {
			t.Errorf("%s gave %q, %v; want %q", tt.script, out, err, tt.want)
		}
	}
}

func TestFailedScriptGivesMessageThenOutputCutAtLimit(t *testing.T) {
	t.Parallel()
	out, err := sandbox.Run(t.Context(), "console.log('0123456789'.repeat(3))\nthrow new Error('boom')", noTools, sandbox.Limits{Output: 25})
	var scriptErr *sandbox.Error
	if !errors.As(err, &scriptErr) || scriptErr.Message != "Error: boom (line 2)" {
		t.Fatalf("Run = %q, %v; want Error: boom (line 2)", out, err)
	}
	if want := "Error: boom (line 2)\n0123\n[... 27 more characters cut]\n"; out != want {
		t.Errorf("result %q, want %q", out, want)
	}

	out, _ = sandbox.Run(t.Context(), "throw new Error('x'.repeat(100))", noTools, sandbox.Limits{Output: 25})
	if want := "Error: xxxxxxxxxxxxxxxxxx\n[... 91 more characters cut]\n"; out != want {
		t.Errorf("result %q, want %q", out, want)
	}
}

func TestScriptPrintingWithoutEndKeepsOnlyOutputLimit(t *testing.T) {
	t.Parallel()
	out, err := sandbox.Run(t.Context(), "for (;;) console.log('x'.repeat(1 << 20))", noTools, sandbox.Limits{Time: 300 * time.Millisecond, Output: 100})
	var scriptErr *sandbox.Error
	if !errors.As(err, &scriptErr) || scriptErr.Message != "script timed out after 300ms" {
		t.Fatalf("Run = %v, want a timeout", err)
	}
	head, closing, ok := strings.Cut(out, "\n")
	if !ok || head != "script timed out after 300ms" || len(closing) > 100+60 || !strings.Contains(closing, "more characters cut]") {
		t.Errorf("result %.200q, want the timeout, then 100 characters and a closing line", out)
	}
}

func TestRunCancelsCallsLeftRunningAndWaitsForThem(t *testing.T) {
	t.Parallel()
	var running atomic.Int32
	block := func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		running.Add(1)
		defer running.Add(-1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	// The call may not have reached block when the Script returns; then
	// this checks only that Run returns at all, which a call left
	// blocked would prevent.
	for range 20 {
		out, err := sandbox.Run(t.Context(), "call('block'); return 'done'", block, sandbox.Limits{})
		if err != nil || out != `"done"`+"\n" {
			t.Fatalf("Run = %q, %v; want the Script's return value", out, err)
		}
		if n := running.Load(); n != 0 {
			t.Fatalf("Run returned with %d calls still running", n)
		}
	}
}

func TestCallPassesArgumentsAsJSONAndDefaultsThemToAnEmptyObject(t *testing.T) {
	t.Parallel()
	echo := func(_ context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(name + " " + string(args))
	}
	out, err := sandbox.Run(t.Context(), `return [await call("a", {n: [1]}), await call("b")]`, echo, sandbox.Limits{})
	if want := `["a {\"n\":[1]}","b {}"]` + "\n"; err != nil || out != want {
		t.Errorf("Run = %q, %v; want %q", out, err, want)
	}
}
