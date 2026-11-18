# sandbox

## Overview

Package `sandbox` runs a Script: a JavaScript program, the body of an async function, in a fresh QuickJS VM of its own (`modernc.org/quickjs`). The VM offers nothing beyond standard JavaScript but `console` and `call`, through which the caller supplies tools. The package knows nothing of MCP or Upstreams: the `gateway` package gives it a `Caller` and turns the result into the answer of `execute` (Code mode, [ADR-0005](../../docs/adr/0005-code-mode-embedded-quickjs-read-only-tools.md)). It uses [`chars`](../chars/README.md) to cut its output.

## Interface

- `func Run(ctx context.Context, script string, caller Caller, limits Limits) (string, error)`: runs `script` under `limits` and returns its result: what it printed, followed by the JSON of the value it returned unless that is `undefined`.
- `type Caller func(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)`: calls the tool named `name` with `args`, the JSON of the arguments the Script passed, and returns the JSON of the value that `call` resolves to. An error rejects the Promise of `call` with an `Error` whose message is the error's.
- `type Limits struct`: what one Script may use. A zero field sets no limit.
  - `Time`: wall time, from the start of `Run`.
  - `Memory`: bytes the VM may allocate.
  - `Stack`: call depth, in the engine's stack slots.
  - `Output`: characters of the result; the rest is cut, and a closing line says how much.
  - `InFlight`: calls running at once; the others wait their turn.
  - `Calls`: calls in all; the next one throws.
- `type Error struct { Message string }`: a Script that failed: it didn't parse, threw, or ran out of a limit. `Message` holds the error, with its line in the Script where known.
- What a Script sees, defined by `prelude`:
  - `console.log`, `console.error` and `console.warn`: all three write one line to the same output, their arguments joined by a space: strings as they are, an `Error` as `String` gives it, other values as JSON, and a value that has no JSON (`undefined`, a function, a cycle) as `String` gives it (`format`).
  - `call(name, args)`: starts a call to the `Caller` and returns a Promise of its result. `args` left `undefined` is sent as `{}`.

## Invariants

- Each `Run` gets a new VM, closed before `Run` returns: no state passes from one Script to the next.
- The Script reaches the host only through `console` and `call`. The host functions behind them (`__sprut_print`, `__sprut_call`) are taken off `globalThis` by the prelude before the Script runs.
- The Script is the body of an async function that starts on the first line of the evaluated source, so the engine's line numbers are the Script's.
- The result is what the Script printed, then the JSON of its return value on a line of its own, unless that value is `undefined` or has no JSON, such as a function. A Script that fails gives an `*Error`, and its result is the error's message on a line of its own, then what it printed. `Run` returns both the result and the `*Error` in that case.
- Only a failure of the Script is an `*Error`. When `ctx` is done, `Run` returns `""` and `ctx`'s error; a failure to set up the VM is a plain wrapped error. `ctx` is checked before `Limits.Time`, so a cancelled request is never reported as a timeout.
- Messages a caller may see in an `*Error`: `script timed out after <Time>`, `the script awaited a promise that never settles`, and the engine's own errors, with ` (line N)` appended where the engine says, and prefixed `uncaught ` when the thrown value is not an `Error` (`describe`). The call limit is one of those engine errors: `a script may make at most <Calls> calls` is the `message` of an `Error` thrown where the Script called, so the Script can catch it, and one it doesn't catch is reported like any other thrown `Error`: `Error: a script may make at most <Calls> calls (line N)`.
- The output is cut in characters at `Limits.Output` as it is written, and ends with the line `[... N more characters cut]` when anything was cut. The buffer is outside the VM's memory limit, which is why it is capped on write and not afterwards.
- A call the Script started and didn't await is made in full: a Script that returns ends only once every call it started has, so that a write is never cut short, though that call's result is lost and its failure unreported. A Script that fails, or runs out of `Limits.Time`, ends at once.
- `Run` returns only once no call is left running: it cancels the Script's context, which stops the calls a failed Script left running, then waits for every call's goroutine. A `Caller` must therefore return once its `ctx` is done, or `Run` never returns.
- A `Caller` runs in a goroutine of its own for each call, up to `Limits.InFlight` at once, so it must be safe for concurrent use. The VM is touched only on the goroutine of `Run`: Promises are settled there (`settleNext`), and `vm.Interrupt` is the one VM method called from another goroutine.
- An error from the `Caller` is thrown where the Script called, with the frame of `call` itself dropped from the stack, so that the reported line is the Script's.
- A `Caller` that returns no error must return valid JSON. Anything else, an empty or nil `json.RawMessage` included, fails the whole Script with the engine's `SyntaxError` as an `*Error`: `JSON.parse` throws while the Promise is being settled, not inside the Script, so a `try`/`catch` around `call` does not catch it.

## Side Effects

- Starts and closes one QuickJS VM for each `Run`, and one goroutine for each call the Script makes.
- Calls the `Caller` with a context that ends when the Script does: on its return, its failure, `Limits.Time` or the end of `ctx`. Whatever the `Caller` does (network, Upstream calls) is the caller's.
- Interrupts the VM (`vm.Interrupt`) when that context ends.
- No disk, network, environment or process access of its own, and none is given to the Script.

## Verification

- Run: `go test ./internal/sandbox/...`
