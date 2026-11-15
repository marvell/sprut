# audit

## Overview

Package `audit` writes the Audit log: one JSONL file per `sprut serve`, holding the Agent's session, every Namespaced tool call that reaches an Upstream and every Script, with timings, outcomes and sizes. Arguments and results are never written, only their sizes; a Script's code is.

The `cli` package makes the `Log` for `sprut serve`, and the `gateway` package writes the events to it. The package uses [`statedir`](../statedir/README.md) to locate and protect the directory, and [`chars`](../chars/README.md) to cut error text.

## Interface

- `type Outcome string` with the constants `OK` (`"ok"`), `Error` (`"error"`: an `isError` result, or a failure) and `Cancelled` (`"cancelled"`: the request's context ended first).
- `type Session struct`: the Agent's session, the first event of an Audit log. Fields `Sprut`, `Agent`, `AgentVersion`, `Protocol`, `CodeMode`.
- `type Call struct`: a Namespaced tool call that reached an Upstream. Fields `Tool`, `Upstream`, `Script` (the number of the Script that made it, or 0 for a direct call), `Took`, `Outcome`, `Err`, `ArgsBytes`, `ResultBytes`.
- `type Script struct`: one call to `execute`. Fields `N` (1 for the session's first Script, and so on), `Took`, `Outcome`, `Err`, `Calls`, `Code`, `ResultBytes`.
- `type Log struct`: the Audit log of one `sprut serve`.
- `func New(lookupEnv config.LookupEnv, launched time.Time, log *slog.Logger) *Log`: the Audit log of a `sprut serve` launched at `launched`, under sprut's state directory. Failures are logged to `log`.
- `func (l *Log) Start(s Session)`: creates the file of the Agent's session and writes `s` to it, unless the `Log` is already started.
- `func (l *Log) Call(c Call)`: writes a `call` event.
- `func (l *Log) Script(s Script)`: writes a `script` event.
- `func (l *Log) Close() error`: closes the file, after which every event is dropped.

## Invariants

- The Audit log never fails its caller. `Start`, `Call` and `Script` return nothing: after any failure to create or write the file, `fail` logs one `audit log disabled` ERROR line to the `slog.Logger`, closes the file and drops every later event.
- A nil `*Log` is valid and drops every event: every method checks `l == nil` first. This is how a `sprut serve` without the Audit log runs.
- The file is created only by `Start`, and events before it are dropped. Only the first `Start` or `Close` counts (`started`): a second `Start`, or a `Start` after `Close`, does nothing, so a `Log` writes at most one file, and at most one `session` event, which is the file's first line.
- `Log` is safe for concurrent use: `mu` guards the file, and each event is written with one `Write` of one whole line. Events are encoded outside the lock (`writeEvent`).
- Every event is one JSON object on one line, starting with `ts` (local time, `2006-01-02T15:04:05.000Z07:00`) and `type` (`session`, `call` or `script`).
  - `session`: `sprut`, `agent`, `agent_version`, `protocol`, `code_mode`.
  - `call`: `tool`, `upstream`, `outcome`, `error` (left out when empty), `args_bytes`, `result_bytes`, `script` (`null` for a direct call, else the Script's number), `ms`.
  - `script`: `script`, `outcome`, `error` (left out when empty), `calls`, `code`, `result_bytes`, `ms`, `code_bytes` (`len(Code)`, in bytes).
- `Took` is written only as whole milliseconds in `ms`, and `Call.Script` only through the event's own `script` field; both carry `json:"-"`.
- `Err` is cut to `errLimit` (1000) characters with `chars.Cut` before it is written.
- No field holds arguments or results. Add sizes, not content, when extending an event.
- The file is `<state directory>/audit/<agent>/<launched>.jsonl`, where `<launched>` is the launch time as `2006-01-02T15-04-05`. If that name is taken, `create` tries `<launched>-2.jsonl`, `<launched>-3.jsonl` and so on; the file is opened with `O_EXCL`, so a session never writes into another session's file.
- `agentDir` makes the Agent's directory name from the name the Agent sent: lowercased, each run of characters outside `[a-z0-9._-]` replaced by `-`, leading and trailing `-` and `.` trimmed, and `unknown` if nothing is left. It is therefore never empty and never a path of its own (`.`, `..` or one with a separator).
- Without a state directory (`statedir.Dir` reports `false`), `New` still returns a `Log`; `Start` then fails with `cannot locate the audit log: HOME is not set` and the `Log` is disabled.

## Side Effects

- `New` has none: it only resolves the state directory through `lookupEnv`.
- `Start` creates `<state directory>/audit/<agent>/` and makes it, and every directory from the state directory down to it, mode `0700` (`statedir.MkdirPrivate`), then creates the session's file with mode `0600`, opened write-only with `O_APPEND`.
- `Start`, `Call` and `Script` append one line each to that file. Nothing is rotated or deleted.
- A failure writes one ERROR record, `audit log disabled`, to the `slog.Logger` given to `New`.
- `Close` closes the file descriptor.

## Verification

- Run: `go test ./internal/audit/...`
