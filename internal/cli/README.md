# cli

## Overview

Package `cli` is the whole sprut program as a function. `Run` takes the
command line, the environment and the standard streams as arguments and
returns the process exit code, so the program can be driven in-process without
touching `os.Args`, the process environment, the real standard streams or
`os.Exit`.

The package builds a `urfave/cli/v3` command tree and delegates the work to
sibling packages: `internal/config` (open and edit the Config),
`internal/gateway` (start, check and serve Upstreams, Logins and Credentials)
and `internal/audit` (the Audit log). It owns only argument parsing, usage
errors, exit codes and the text printed to stdout and stderr.

Command tree:

| Command | Source | What it does |
| --- | --- | --- |
| `sprut` | `cli.go` | Prints help to stderr and exits 2; never starts the server. |
| `sprut serve` | `serve.go` | Runs the gateway as a stdio MCP server. |
| `sprut upstream` | `upstream.go` | Prints help to stderr and exits 2. |
| `sprut upstream add` | `upstream_add.go` | Adds an Upstream to the Config, creating the Config if there is none. |
| `sprut upstream remove` | `upstream_remove.go` | Removes an Upstream from the Config and deletes its Credentials. |
| `sprut upstream enable` / `disable` | `upstream_switch.go` | Switches an Upstream on or off without removing it. |
| `sprut upstream list` | `upstream.go` | Lists every Upstream with its state and the state of its Credentials. |
| `sprut upstream login` | `upstream_login.go` | Logs in to an OAuth Upstream and stores its Credentials. |
| `sprut upstream logout` | `upstream_logout.go` | Deletes the Credentials of an OAuth Upstream. |

## Interface

Exported symbols:

- `func Run(ctx context.Context, args, env []string, stdin io.Reader, stdout, stderr io.Writer) int`
  (`cli.go`): executes sprut. `args[0]` is the program name, `env` is in
  `os.Environ` form. Returns the process exit code.
- `var Version string` (`version.go`): set at release time with
  `-ldflags "-X github.com/marvell/sprut/internal/cli.Version=..."`. When
  empty, the unexported `version()` falls back to the main module version from
  `debug.ReadBuildInfo`, then to `(devel)`.

Exit codes returned by `Run`:

- `0`: success, including `--version` and help asked for with `-h` or
  `--help`.
- `1`: any error that is not an `ExitCoder` (printed to stderr as
  `sprut: <error>`), `serve --dry-run` when `gw.SkippedCount() > 0`, and
  `upstream list` when `gateway.AllWork(reports)` is false.
- `2`: every usage error (`usageError`).

Root flags (`cli.go`, `serve.go:configFlag`):

- `--version`: prints `sprut version <version>` to stdout and exits 0. It is
  `Local`, so it is a flag of the root command only: `sprut --version serve`
  prints the version, and `sprut serve --version` is a usage error (exit 2).
- `--config`, `-c`: path to the config file. It is not `Local`, so every
  subcommand takes it too, before or after the command.

`serve` flags (`serve.go:serveCommand`):

- `--startup-timeout` (default `30s`, must be positive): how long each
  Upstream may take to start.
- `--verbose`, `-v`: log at DEBUG level.
- `--dry-run`: start every Upstream, then shut down; exit 1 if any failed.
- `--code-mode`: also offer the `execute` tool (`gateway.CodeMode`).
- `--audit`: write the Audit log (`audit.New`).
- `--script-time-limit` (hidden, default `gateway.ScriptTimeLimit`, must be
  positive): for tests only.

`upstream add` (`upstream_add.go:addCommand`):

- Usage: `sprut upstream add [options] <name> -- <command> [args...]` or
  `sprut upstream add [options] <name> <http(s) url>`.
- `--env`, `-e` `KEY=VALUE`: environment variable of a stdio Upstream.
- `--header`, `-H` `'Name: value'`: header of an HTTP Upstream.
- `--oauth-client-id`, `--oauth-client-secret`, `--oauth-scope`: OAuth
  settings of an HTTP Upstream.
- `--disabled`: add the Upstream switched off.
- `--force`: replace an Upstream of that name, keeping its place in the
  Config.
- `--no-check`, `--startup-timeout`: control the check of the written
  Upstream.

`upstream remove <name>` (`upstream_remove.go:removeCommand`): no flags of
its own.

`upstream enable <name>` and `upstream disable <name>`
(`upstream_switch.go:switchCommand`): `enable` also takes `--no-check` and
`--startup-timeout`; `disable` takes no flags.

`upstream list` (`upstream.go:listCommand`):

- `--check`: start every enabled Upstream, as `serve` would, and say whether
  it works.
- `--startup-timeout`.
- Output: a table with the columns `NAME`, `TRANSPORT`, `TARGET`, `STATE`,
  `AUTH`. `STATE` is `enabled`, `disabled`, `skipped: <reason>`, or with
  `--check` `ok (<n> tools)`, `needs login` or `failed: <error>`. `AUTH` is
  `-`, `no credentials`, `<reason>; <remedy>`, `error: <error>` or
  `logged in, access token ...`.

`upstream login <upstream>` (`upstream_login.go:loginCommand`):

- `--no-browser`: only print the authorization URL.
- `--verbose`, `-v`: log at DEBUG level.

`upstream logout <upstream>` (`upstream_logout.go:logoutCommand`): no flags
of its own.
Prints `<name>: logged out` or `<name>: no credentials`.

## Invariants

- Bare `sprut` never starts the server: the root `Action` returns
  `usageError` (`cli.go:Run`). `sprut upstream` without a subcommand does the
  same (`upstream.go:upstreamCommand`).
- `urfave/cli` never calls `os.Exit`: the root `ExitErrHandler` is a no-op
  and `Run` turns errors into exit codes itself (`cli.go:Run`).
- A usage error and the help shown with it go to stderr, never stdout:
  `usageError` points the root `Writer` at `ErrWriter` before showing help,
  because help on stdout would corrupt the MCP channel of `serve`
  (`cli.go:usageError`). Help asked for with `-h` or `--help` goes to stdout,
  the root's `Writer`, and exits 0 without running the command.
- `--version` wins over a subcommand: it is handled in the root `Before`, not
  in `Action`. `urfave/cli`'s own version flag is hidden (`HideVersion`)
  because its `-v` alias belongs to `serve --verbose` (`cli.go:Run`).
- Every command in the tree is strict (`cli.go:strictUsage`): a bad flag or
  flag value, a missing argument, an unknown command and any positional
  argument beyond those a command declares are usage errors with exit 2. An
  unknown help topic is reported through `helpErr`.
- A slice flag's value is never split at commas
  (`DisableSliceFlagSeparator` in `cli.go:strictUsage`).
- Every `upstream` leaf command sets `HideHelpCommand`, because `h` and
  `help` are valid Upstream names; `--help` and `-h` stay
  (`upstream.go:upstreamCommand`).
- Duration flags `--startup-timeout` and `--script-time-limit` reject values
  that are not above zero (`serve.go:positive`).
- `serve` treats the Agent going away (EOF on stdin, which `gw.Serve` reports
  as nil) and a cancelled `ctx` as normal ends of a session; any other error
  from `gw.Serve` is returned as `serving the agent: ...`
  (`serve.go:serveCommand`).
- `upstream add` takes the stdio form when its command line has a `--`, which
  must come right after the name and be followed by a command; a `--` anywhere
  else is a usage error, not a fallback to the URL form. Without a `--` it
  takes exactly one `http(s)` URL, with the scheme matched in any case
  (`upstream_add.go:definition`, `isHTTPURL`).
  `addCommand` receives sprut's whole command line because `urfave/cli` drops
  the `--`.
- A flag of `upstream add` that does not apply to the Upstream's Transport is
  a usage error: `-e/--env` is stdio only; `-H/--header` and the
  `--oauth-*` flags are HTTP only (`upstream_add.go:transportFlags`,
  `setFields`). `--oauth-client-secret` needs `--oauth-client-id`, and a set
  OAuth flag must not be empty.
- `-e` values must be `KEY=VALUE` with a non-empty key; `-H` values must be
  `Name: value` with a non-empty name without spaces or tabs. The last value
  of a repeated key wins (`upstream_add.go:envVars`, `headers`, `pairs`).
- `upstream add` on an existing name fails with `config.ErrExists` wrapped as
  `...; use --force to replace it` (`upstream_add.go:addCommand`).
- The check after `add` and `enable` never fails the command: the Config
  stays written whatever `gateway.Check` finds, and nothing is checked that
  the Gateway would not start (`upstream.go:checkWritten`).
- `upstream remove` orders its steps (`upstream_remove.go:removeCommand`):
  `auths.Located()` runs before the Config changes, so a Store that cannot be
  found leaves the Config as it was; `auths.Forget` runs only after the
  Config is saved, so a Config that cannot be saved keeps the Upstream with
  its Credentials.
- `upstream remove` can exit 1 after it has changed the Config: when
  `auths.Forget` fails, the Config is already saved and
  `removed upstream "<name>"` already printed, and the command returns
  `upstream "<name>": <error>`. The Upstream is gone from the Config and its
  Credentials may still be on disk (`upstream_remove.go:removeCommand`).
- `enable` and `disable` on an Upstream already in the requested state print
  `upstream "<name>" is already <state>` and write nothing
  (`upstream_switch.go:switchCommand`).
- `upstream list` tolerates a missing Config (`fs.ErrNotExist`) and prints
  nothing when there are no entries. Credentials that cannot be read show as
  `error: ...` in `AUTH` and do not fail the list (`upstream.go:listCommand`,
  `auth`).
- `upstream login` and `logout` resolve a disabled Upstream too, so that it
  can be enabled logged in (`upstream_login.go:upstreamAuth`).
- The login remedy text is `run: sprut upstream login [-c <path>] <name>`,
  with the Config path quoted for a POSIX shell when it needs quoting
  (`upstream_login.go:loginRemedy`, `shellQuote`).

## Side Effects

- Standard streams: `Run` reads only the `stdin` it is given and writes only
  to the `stdout` and `stderr` it is given. In `serve`, `stdin` and `stdout`
  are handed to `gw.Serve` as the MCP channel; logs go to `stderr` as logfmt
  through `slog.NewTextHandler` (`serve.go:newLogger`).
- Config file reads: `config.Open` in `serve`, `upstream list`,
  `upstream login` and `upstream logout` (`serve.go:openConfig`,
  `upstream.go:listCommand`).
- Config file writes: `config.Edit` in `upstream add` (`doc.Add`, creating
  the Config if there is none), `upstream remove` (`doc.Remove`) and
  `upstream enable` / `disable` (`doc.SetDisabled`).
- Starting Upstreams: `gateway.Start` in `serve`; `gateway.Reports` with
  check options in `upstream list --check`; `gateway.Check` after
  `upstream add` and `upstream enable` unless `--no-check`
  (`upstream.go:checkWritten`). Plain `upstream list` starts no Upstream and
  contacts no server.
- Credentials Store: opened with `gateway.OpenAuths(env, loginRemedy)`
  (`upstream_login.go:openAuths`). `upstream login` stores Credentials through
  `Auth.Login` and may open a browser unless `--no-browser`;
  `upstream logout` deletes them through `Auth.Logout`; `upstream remove`
  deletes them through `Auths.Forget`.
- Audit log: `serve --audit` creates an `audit.Log` with `audit.New` and
  closes it when the command returns; the flag documents the destination as
  `$XDG_STATE_HOME/sprut/audit/<agent>/<start time>.jsonl`
  (`serve.go:serveCommand`).
- Notices: every Config change prints what changed on stdout and
  `note: running agents see this change only after their sprut restarts` on
  stderr (`upstream.go:changed`). Config warnings for the written Upstream go
  to stderr (`upstream.go:warn`).
- Package state: `Version` is the only package-level variable meant to be
  set; `transportFlags` and `upstreams` (`upstream_add.go`) are tables that
  nothing writes after initialisation. `Run` itself does not read `os.Args`,
  the process environment or call `os.Exit`.

## Verification

- Run: `go test ./internal/cli/...`
