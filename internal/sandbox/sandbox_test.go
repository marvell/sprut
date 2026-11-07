package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marvell/sprut/internal/sandbox"
)

// run runs script with no limits and fails the test if it fails.
func run(t *testing.T, script string) string {
	t.Helper()
	out, err := sandbox.Run(t.Context(), script, sandbox.Limits{})
	if err != nil {
		t.Fatalf("Run(%q): %v", script, err)
	}
	return out
}

// runFailing runs script under limits and returns the *sandbox.Error it
// must fail with.
func runFailing(t *testing.T, script string, limits sandbox.Limits) *sandbox.Error {
	t.Helper()
	out, err := sandbox.Run(t.Context(), script, limits)
	var scriptErr *sandbox.Error
	if !errors.As(err, &scriptErr) {
		t.Fatalf("Run(%q) = %q, %v; want a *sandbox.Error", script, out, err)
	}
	return scriptErr
}

func TestScriptSeesNoHostAPIBeyondConsole(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"fetch", "require", "std", "os", "print", "process", "module", "exports",
		"setTimeout", "setInterval", "setImmediate", "queueMicrotask",
		"XMLHttpRequest", "WebSocket", "Deno", "Bun", "__sprut_print",
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
	err := runFailing(t, `await import("os")`, sandbox.Limits{})
	if !strings.Contains(err.Message, "could not load module") {
		t.Errorf("error = %q, want it to say the module could not load", err.Message)
	}
	err = runFailing(t, `import * as os from "os"`, sandbox.Limits{})
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
		err := runFailing(t, tt.script, sandbox.Limits{})
		if err.Message != tt.want {
			t.Errorf("%q failed with %q, want %q", tt.script, err.Message, tt.want)
		}
		wantOutput := "before\n"
		if strings.HasPrefix(tt.want, "SyntaxError") {
			wantOutput = "" // nothing runs
		}
		if err.Output != wantOutput {
			t.Errorf("%q kept output %q, want %q", tt.script, err.Output, wantOutput)
		}
	}
}

func TestScriptStoppedAtTimeLimit(t *testing.T) {
	t.Parallel()
	for _, script := range []string{"for (;;) {}", "await 1; for (;;) {}"} {
		err := runFailing(t, "console.log('looping')\n"+script, sandbox.Limits{Time: 100 * time.Millisecond})
		if err.Message != "script timed out after 100ms" || err.Output != "looping\n" {
			t.Errorf("%q: error %q with output %q, want a timeout after its output", script, err.Message, err.Output)
		}
	}
}

func TestScriptFailsAtMemoryLimit(t *testing.T) {
	t.Parallel()
	err := runFailing(t, "const a = []; for (;;) a.push('x'.repeat(1024) + a.length)", sandbox.Limits{Memory: 16 << 20})
	if !strings.Contains(err.Message, "out of memory") {
		t.Errorf("error = %q, want out of memory", err.Message)
	}
}

func TestCancelledRunStopsScriptAndReturnsContextError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	out, err := sandbox.Run(ctx, "for (;;) {}", sandbox.Limits{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %q, %v; want context.Canceled", out, err)
	}
}
