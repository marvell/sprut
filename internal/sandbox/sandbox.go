// Package sandbox runs a Script: a JavaScript program, the body of an async
// function, in a fresh QuickJS VM of its own that offers nothing beyond
// standard JavaScript but console and call, through which the caller
// supplies tools. It knows nothing of MCP or Upstreams.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// Caller calls the tool named name with args, the JSON of the arguments the
// Script passed, and returns the JSON of the value call resolves to. An
// error rejects call's Promise with an Error whose message is the error's.
// A Caller must return once ctx is done: Run waits for every call.
type Caller func(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)

// The host functions that console and call go through. The prelude takes
// them off the global object before the Script runs.
const (
	printFunc = "__sprut_print"
	callFunc  = "__sprut_call"
)

// inFlight is how many calls of one Script run at a time; the others wait
// their turn.
const inFlight = 1

// prelude defines console, formatting its arguments like console.log in
// Node: strings as they are, other values as JSON. It also defines call,
// which starts a call through the host and returns a Promise, and its
// value is the function that settles that Promise once the call ends: with
// the value of the JSON the call returned, or else with an Error saying
// what went wrong, thrown where the Script called.
const prelude = `(() => {
	const print = globalThis.` + printFunc + `;
	const start = globalThis.` + callFunc + `;
	delete globalThis.` + printFunc + `;
	delete globalThis.` + callFunc + `;
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

	const calls = new Map();
	globalThis.call = (name, args) => {
		// Where the Script called, for the line of an Error the call ends
		// in. Every eval is "<eval>" to the engine, prelude and Script
		// alike, so this frame is dropped from the stack when it is used.
		const site = new Error();
		return new Promise((resolve, reject) => {
			const id = start(String(name), args === undefined ? "{}" : JSON.stringify(args));
			calls.set(id, { resolve, reject, site });
		});
	};
	return (id, error, json) => {
		const { resolve, reject, site } = calls.get(id);
		calls.delete(id);
		if (error === null) {
			resolve(JSON.parse(json));
			return;
		}
		const e = new Error(error);
		e.stack = site.stack.split("\n").slice(1).join("\n");
		reject(e);
	};
})();`

// Run runs script under limits and returns its result: what it printed,
// followed by the JSON of the value it returned unless that is undefined.
// A Script that fails gives an *Error, and its result is the error's
// message on a line of its own, then what it printed. Either way the result
// is cut at the output limit. The Script's calls go to caller, under a
// context that ends with the Script, so that Run returns only once none is
// left running. Once ctx is done, the Script is stopped and Run returns
// ctx's error.
func Run(ctx context.Context, script string, caller Caller, limits Limits) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var (
		runCtx context.Context
		cancel context.CancelFunc
	)
	if limits.Time > 0 {
		runCtx, cancel = context.WithTimeout(ctx, limits.Time)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	calls := &calls{ctx: runCtx, caller: caller, ended: make(chan ended), turns: make(chan struct{}, inFlight)}
	defer calls.wg.Wait()
	defer cancel()

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
	err = vm.RegisterHostFunc(callFunc, func(args []any) (any, error) {
		name, _ := args[0].(string) // always both strings, from the prelude
		arguments, _ := args[1].(string)
		return calls.start(name, arguments), nil
	})
	if err != nil {
		return "", fmt.Errorf("setting up call: %w", err)
	}
	settle, err := vm.EvalValue(prelude, quickjs.EvalGlobal)
	if err != nil {
		return "", fmt.Errorf("setting up the globals: %w", err)
	}
	defer settle.Free()
	calls.settle = settle

	returned, err := run(vm, script, calls)
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case runCtx.Err() != nil:
		return out.failed(fmt.Sprintf("script timed out after %s", limits.Time))
	case err != nil:
		return out.failed(describe(err))
	}
	if returned != "" {
		out.write(returned)
		out.write("\n")
	}
	return out.String(), nil
}

// failed is the result and the *Error of a Script that failed with message
// after printing o.
func (o *output) failed(message string) (string, error) {
	res := output{limit: o.limit, cut: o.cut}
	res.write(message)
	if printed := o.text.String(); printed != "" {
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

// run runs script on vm as the body of an async function, settling its
// calls as they end, and returns the JSON of the value it resolves to, or
// "" for undefined.
func run(vm *quickjs.VM, script string, calls *calls) (string, error) {
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

	for {
		if _, err := vm.ExecutePendingJobs(); err != nil {
			return "", err
		}
		switch {
		case failed != nil:
			return "", failed
		case settled:
			return returned, nil
		case calls.running == 0:
			return "", errors.New("the script awaited a promise that never settles")
		}
		if err := calls.settleNext(); err != nil {
			return "", err
		}
	}
}

// calls runs a Script's calls to its Caller, each in a goroutine of its
// own, and settles their Promises on the VM's goroutine.
type calls struct {
	ctx    context.Context // the Script's: done once it ends
	caller Caller
	settle quickjs.Value // the prelude's settling function
	ended  chan ended
	turns  chan struct{} // one token per call running at the Caller

	next    int            // the id of the next call
	running int            // calls started and not yet settled
	wg      sync.WaitGroup // the calls' goroutines, which return once ctx is done
}

// ended is the outcome of the call with id.
type ended struct {
	id    int
	value json.RawMessage
	err   error
}

// start starts a call to the tool name with args, the JSON of its
// arguments, and returns its id.
func (c *calls) start(name, args string) int {
	id := c.next
	c.next++
	c.running++
	c.wg.Go(func() {
		select {
		case c.turns <- struct{}{}:
		case <-c.ctx.Done():
			return
		}
		value, err := c.caller(c.ctx, name, json.RawMessage(args))
		<-c.turns
		select {
		case c.ended <- ended{id, value, err}:
		case <-c.ctx.Done():
		}
	})
	return id
}

// settleNext waits for a call to end and settles its Promise, or returns
// the Script's context's error once that is done.
func (c *calls) settleNext() error {
	select {
	case e := <-c.ended:
		c.running--
		var message any // null, unless the call failed
		if e.err != nil {
			message = e.err.Error()
		}
		_, err := c.settle.Call(quickjs.UndefinedValue, e.id, message, string(e.value))
		return err
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
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
