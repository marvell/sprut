// Package sandbox runs a Script: a JavaScript program, the body of an async
// function, in a fresh QuickJS VM of its own that offers nothing beyond
// standard JavaScript but console. It knows nothing of MCP or Upstreams.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"modernc.org/quickjs"
)

// Limits bound what one Script may use. A zero field sets no limit.
type Limits struct {
	Time   time.Duration // wall time, from the start of Run
	Memory uintptr       // bytes the VM may allocate
	Stack  uintptr       // call depth, in the engine's stack slots: about one per call
	Output int           // characters of the result; the rest is cut, and a closing line says how much
}

// Error is a Script that failed: it didn't parse, threw, or ran out of a
// limit. The result Run returns with it says so too, and holds the output.
type Error struct {
	Message string // the error, with its line in the Script where known
}

func (e *Error) Error() string { return e.Message }

// printFunc is the host function console writes through. The prelude takes
// it off the global object before the Script runs.
const printFunc = "__sprut_print"

// prelude defines console, formatting its arguments like console.log in
// Node: strings as they are, other values as JSON.
const prelude = `(() => {
	const print = globalThis.` + printFunc + `;
	delete globalThis.` + printFunc + `;
	const format = (v) => {
		if (typeof v === "string") return v;
		if (v instanceof Error) return String(v);
		try {
			const json = JSON.stringify(v);
			if (json !== undefined) return json;
		} catch {}
		return String(v);
	};
	const write = (...args) => { print(args.map(format).join(" ")); };
	globalThis.console = { log: write, error: write, warn: write };
})();`

// Run runs script under limits and returns its result: what it printed,
// followed by the JSON of the value it returned unless that is undefined.
// A Script that fails gives an *Error, and its result is the error's
// message on a line of its own, then what it printed. Either way the result
// is cut at the output limit. Once ctx is done, the Script is stopped and
// Run returns ctx's error.
func Run(ctx context.Context, script string, limits Limits) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	runCtx := ctx
	if limits.Time > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, limits.Time)
		defer cancel()
	}

	vm, err := quickjs.NewVM()
	if err != nil {
		return "", fmt.Errorf("starting the javascript vm: %w", err)
	}
	defer func() { _ = vm.Close() }()
	if limits.Memory > 0 {
		vm.SetMemoryLimit(limits.Memory)
	}
	if limits.Stack > 0 {
		vm.SetMaxStackSize(limits.Stack)
	}
	// Interrupt is the one VM method safe from another goroutine, and does
	// nothing once the VM is closed.
	stop := context.AfterFunc(runCtx, vm.Interrupt)
	defer stop()

	// Capped as it is written: the buffer is outside the VM's memory limit.
	out := output{limit: limits.Output}
	err = vm.RegisterHostFunc(printFunc, func(args []any) (any, error) {
		if len(args) > 0 {
			text, ok := args[0].(string) // always, from the prelude
			if !ok {
				text = fmt.Sprint(args[0])
			}
			out.write(text)
		}
		out.write("\n")
		return nil, nil
	})
	if err != nil {
		return "", fmt.Errorf("setting up console: %w", err)
	}
	if _, err := vm.Eval(prelude, quickjs.EvalGlobal); err != nil {
		return "", fmt.Errorf("setting up console: %w", err)
	}

	returned, err := run(vm, script)
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case runCtx.Err() != nil:
		return failed(fmt.Sprintf("script timed out after %s", limits.Time), &out)
	case err != nil:
		return failed(describe(err), &out)
	}
	if returned != "" {
		out.write(returned)
		out.write("\n")
	}
	return out.String(), nil
}

// failed is the result and the *Error of a Script that failed with message
// after printing out.
func failed(message string, out *output) (string, error) {
	res := output{limit: out.limit, cut: out.cut}
	res.write(message)
	if printed := out.text.String(); printed != "" {
		res.write("\n")
		res.write(printed)
	}
	return res.String(), &Error{Message: message}
}

// output is text kept up to limit characters, or all of it when limit is
// 0. Beyond the limit, characters are only counted.
type output struct {
	text  strings.Builder
	limit int
	kept  int // characters in text
	cut   int // characters dropped
}

func (o *output) write(s string) {
	if o.limit <= 0 {
		o.text.WriteString(s)
		return
	}
	room := o.limit - o.kept
	for i := range s { // i is the byte offset of each character
		if room == 0 {
			o.text.WriteString(s[:i])
			o.kept = o.limit
			o.cut += utf8.RuneCountInString(s[i:])
			return
		}
		room--
	}
	o.text.WriteString(s)
	o.kept = o.limit - room
}

// String is the text kept, followed by a line saying how much was cut, if
// any.
func (o *output) String() string {
	text := o.text.String()
	if o.cut == 0 {
		return text
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + fmt.Sprintf("[... %d more characters cut]\n", o.cut)
}

// run runs script on vm as the body of an async function, and returns the
// JSON of the value it resolves to, or "" for undefined.
func run(vm *quickjs.VM, script string) (string, error) {
	// The body starts on the first line, so the Script's line numbers are
	// the engine's.
	promise, err := vm.EvalValue("(async function () {"+script+"\n})().then((v) => v === undefined ? undefined : JSON.stringify(v))", quickjs.EvalGlobal)
	if err != nil {
		return "", err
	}
	defer promise.Free()

	var (
		settled  bool
		returned string
		failed   error
	)
	then, err := promise.Then(func(v quickjs.Value) {
		settled = true
		if s, err := v.Any(); err == nil {
			returned, _ = s.(string)
		}
	}, func(v quickjs.Value) {
		settled = true
		failed = quickjs.ErrorFromValue(v)
	})
	if err != nil {
		return "", err
	}
	defer then.Free()

	if _, err := vm.ExecutePendingJobs(); err != nil {
		return "", err
	}
	if failed != nil {
		return "", failed
	}
	if !settled {
		return "", errors.New("the script awaited a promise that never settles")
	}
	return returned, nil
}

// frameLine finds the line of the first stack frame in the Script.
var frameLine = regexp.MustCompile(`<eval>:(\d+):\d+`)

// describe is err's message with the Script line it happened on, where the
// engine says.
func describe(err error) string {
	var js *quickjs.Error
	if !errors.As(err, &js) {
		return err.Error()
	}
	msg := js.Error()
	if js.Name == "" {
		msg = "uncaught " + msg // a thrown value that is not an Error
	}
	line := js.LineNumber
	if line == 0 {
		if m := frameLine.FindStringSubmatch(js.Stack); m != nil {
			line, _ = strconv.Atoi(m[1])
		}
	}
	if line > 0 {
		msg += fmt.Sprintf(" (line %d)", line)
	}
	return msg
}
