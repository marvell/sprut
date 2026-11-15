# gateway

## Overview

Package `gateway` connects to Upstreams (MCP servers named in the Config) and
serves their tools to one Agent as Namespaced tools, over a single MCP server.
It is the core of `sprut serve` and of the `sprut upstream` commands in
`internal/cli`, which is its only caller.

It has four jobs:

- **Serving** (`gateway.go`): start every Upstream concurrently, register each
  tool as `<upstream>__<tool>`, proxy calls, and stop the Upstreams on `Close`.
- **Transports** (`stdio.go`, `http.go`): launch a stdio Upstream as a child
  process in its own process group, or connect to an HTTP Upstream over
  Streamable HTTP.
- **OAuth** (`auth.go`, `oauth.go`, `login.go`, `httplog.go`): decide whether an
  Upstream is an OAuth Upstream, send and renew its stored Credentials in
  serve, and run the interactive Login.
- **Diagnostics and Code mode** (`check.go`, `report.go`, `execute.go`): start
  an Upstream only to say how it fares, report the state of its Credentials,
  and serve the `execute` tool that runs a Script in `internal/sandbox`.

It depends on the sibling packages `internal/config` (Upstreams and entries),
`internal/credentials` (the Store, token sources, renewal), `internal/audit`
(the Audit log) and `internal/sandbox` (Script execution), and on the MCP
go-sdk (`mcp`, `jsonrpc`, `auth`, `oauthex`), `golang.org/x/oauth2` and
`golang.org/x/net/http/httpproxy`. Domain
terms (Upstream, Agent, Namespaced tool, Login, Credentials, Script) are defined
in `GLOSSARY.md` at the repository root.

| File         | Contents                                                                       |
| ------------ | ------------------------------------------------------------------------------ |
| `gateway.go` | `Gateway`, `Start`, `Serve`, `Close`, tool registration, call proxying         |
| `execute.go` | Code mode: the `execute` tool, Script limits, the value a Script gets back     |
| `check.go`   | `Check`, `Health`, `HealthState`                                               |
| `report.go`  | `Reports`, `Report`, `AllWork`, `AuthStatus`, `AuthState`                      |
| `stdio.go`   | stdio Upstream process: launch, state machine, stop sequence, stderr logging   |
| `http.go`    | Streamable HTTP transport: proxy, Config headers, redirects, HTTP+SSE probe    |
| `auth.go`    | `Auths`, `Auth`, OAuth eligibility (`checkOAuth`), `Status`, `Logout`          |
| `oauth.go`   | Serve-time OAuth handler: bearer token, renewal, issuer check, error reduction |
| `login.go`   | `Auth.Login`: loopback callback server, browser, client registration           |
| `httplog.go` | DEBUG log of a Login's HTTP exchanges, with secrets redacted                   |

## Interface

Serving:

- `func Start(ctx context.Context, configured []config.Upstream, auths *Auths, o *StartOptions, f Features) *Gateway`:
  launches every Upstream concurrently, collects their tools and returns a
  `Gateway` ready to serve. It returns no error: an Upstream that fails or
  times out is skipped.
- `type Gateway struct` (no exported fields):
  - `func (g *Gateway) Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error`:
    serves MCP to one Agent until it disconnects or `ctx` is done.
  - `func (g *Gateway) Close()`: stops every Upstream and waits for abandoned
    starts and for Credential renewals in progress.
  - `func (g *Gateway) SkippedCount() int`: how many Upstreams failed or timed
    out at startup.
- `type StartOptions struct { Env []string; StartupTimeout time.Duration; Version string; Log *slog.Logger }`:
  how `Start`, `Check` and `Reports` start Upstreams. `Env` is the environment
  Upstream processes inherit, `StartupTimeout` what each Upstream gets to
  connect and list its tools.
- `type Features struct { CodeMode *CodeMode; Audit *audit.Log }`: opt-in
  features, each off when nil.
- `type CodeMode struct { ScriptTimeLimit time.Duration }`: turns on Code mode.
- `const ScriptTimeLimit = 50 * time.Second`: how long a Script may run in
  production.

Diagnostics:

- `func Check(ctx context.Context, u config.Upstream, auths *Auths, o *StartOptions) Health`:
  starts `u` as `Start` does, lists its tools, stops it, and says how it fared.
- `type Health struct { State HealthState; Tools int; Err error; Remedy string }`.
- `type HealthState int`: `HealthWorks` (zero value), `HealthNeedsLogin`,
  `HealthFailed`.
- `func Reports(ctx context.Context, entries []config.Entry, auths *Auths, checkOpts *StartOptions) []Report`:
  one `Report` per entry, in order; with a non-nil `checkOpts` it also starts
  every entry the Gateway starts.
- `type Report struct { Entry config.Entry; Auth AuthStatus; Health *Health }`.
- `func AllWork(reports []Report) bool`: whether every checked entry works.
- `type AuthStatus struct { State AuthState; Expiry time.Time; Expired bool; Renewable bool; Err error; Reason, Remedy string }`.
- `type AuthState int`: `AuthNotOAuth` (zero value), `AuthNoCredentials`,
  `AuthLoggedIn`, `AuthDead`, `AuthUnreadable`.

Logins and Credentials:

- `func OpenAuths(env []string, remedy func(config.Upstream) string) *Auths`:
  opens the Credentials Store that `env` points at. `remedy` returns the command
  the user runs to log in to an Upstream.
- `type Auths struct` (no exported fields), shared by every `Auth` of a process:
  - `func (a *Auths) Located() error`: why the Store can't be located, or nil.
  - `func (a *Auths) Forget(ctx context.Context, name string) (bool, error)`:
    deletes the Credentials stored under an Upstream name, whether or not the
    Config still has it.
  - `func (a *Auths) Wait()`: waits for Credential renewals in progress.
  - `func (a *Auths) For(u config.Upstream) *Auth`.
- `type Auth struct` (no exported fields), the Login and Credentials of one
  Upstream:
  - `func (a *Auth) Login(ctx context.Context, browser bool, prompt io.Writer, version string, log *slog.Logger) error`:
    runs the interactive Login and stores the Credentials it gets.
  - `func (a *Auth) Logout(ctx context.Context) (bool, error)`.
  - `func (a *Auth) Status() AuthStatus`.
  - `func (a *Auth) Remedy() string`.

Unexported symbols that carry the contract:

- `separator = "__"` and `validToolName = ^[a-zA-Z0-9_-]{1,64}$` (`gateway.go`).
- `executeTool = "execute"` and the Script limits `scriptMemory`,
  `scriptCallDepth`, `scriptOutput`, `scriptInFlight`, `scriptCalls`
  (`execute.go`).
- `hinted` (`interface { error; Hint() string }`), implemented by
  `needsLoginError` (`gateway.go`, `oauth.go`).
- `stdioUpstream` with its `state`/`event`/`next` state machine, and
  `stopGrace`, `drainGrace`, `exitGrace`, `maxStderrLine` (`stdio.go`).
- `checkOAuth`, `sameOrigin`, `sameOriginRedirects`, `maxRedirects`,
  `headerTransport` (`auth.go`, `http.go`).
- `storedCredentials`, `bearerTransport`, `errorBodies`, `withResource`,
  `oauthClient` (`oauth.go`); `callbackServer`, `issuerRecorder`,
  `authorizeOnce` (`login.go`); `exchangeLog`, `secretFields` (`httplog.go`).

This package has no subdirectories.

## Invariants

Naming and the tool list:

- A Namespaced tool is named exactly `<upstream>` + `__` + `<tool>`; the rest of
  its definition is the Upstream's, unchanged (`addTool`, `separator`).
- A Namespaced name that doesn't match `^[a-zA-Z0-9_-]{1,64}$` is skipped,
  never truncated or renamed (`validToolName`). A tool the SDK rejects (it
  panics in `AddTool`) is recovered and skipped too. Either way the Upstream's
  other tools are still served (`addTools`).
- `execute` holds no separator, so it never collides with a Namespaced tool
  (`executeTool`). It is not in the catalog, so a Script cannot call it.
- The tool list is fixed for the session: the server advertises only the tools
  capability, and the catalog is written only during `Start` and read without a
  lock after it (`Gateway.catalog`).

Startup and shutdown:

- A failed Upstream is never fatal. `Start` has no error return; the Upstream is
  logged, counted by `SkippedCount`, and named with its error and remedy in the
  server's instructions to the Agent (`instructions`, `withHint`).
- Upstreams start concurrently, but results, instructions, tool registration
  and the INFO `upstream ready` lines are in Config order (`upstreams.start`,
  `Start`). The WARN `upstream failed; skipped` line is logged by each
  Upstream's own goroutine as it fails, so in the order they fail.
- Each Upstream gets its own `StartupTimeout` to connect and list its tools.
  Past it the error is `startup timed out after <timeout>`, and `startOne`
  returns without waiting for the connect to finish. A session that an
  abandoned connect still produces is closed by its goroutine.
- `StartOptions` and its `Log` must be non-nil for `Start` and `Check`, which
  dereference them without a check. For `Reports` a nil `checkOpts` is the
  valid "do not check" value; a non-nil one needs a non-nil `Log` too. No
  default is applied to `StartupTimeout` or to `CodeMode.ScriptTimeLimit`.
- `auths` must be non-nil for `Start`, `Check` and `Reports`, and so must the
  `remedy` given to `OpenAuths`: neither is checked. A nil `*Auths` panics in
  `upstreams.close` (`Auths.Wait`), when an HTTP Upstream connects
  (`Auths.For`) and in `statusOf`; a nil `remedy` panics in `Auth.Remedy`.
- The caller must call `Gateway.Close`. It closes all sessions concurrently,
  then waits for abandoned starts, then for renewals (`upstreams.close`).
  `Check` and `Reports` stop what they start themselves (`check` defers
  `close`).
- `Gateway.Close` does not close the Audit log in `Features.Audit`: the
  Gateway only calls `Start`, `Call` and `Script` on it, and whoever made the
  `audit.Log` closes it. `Check` and `Reports` write no Audit log.
- `Serve` never closes the reader and writer it is given (`io.NopCloser`,
  `nopWriteCloser`).

Calls:

- A call goes to the Upstream under the tool's original name with the Agent's
  raw arguments (`namespacedTool.call`).
- A call to a tool the Gateway doesn't serve gets an `isError` result
  `unknown tool "<name>"`, not a protocol error (`answerUnknownTools`).
- A call that fails on the way to the Upstream becomes an `isError` result
  `call to upstream "<name>" failed: ...`. The Upstream's own JSON-RPC error is
  returned as it is, and so is the error of a cancelled call (`answered`,
  `isRejected`).
- The Gateway proxies no interactive input: the client disables
  `MultiRoundTrip`, and a result that asks for input becomes an `isError`
  result (`newClient`, `namespacedTool.call`).
- Every `isError` result the Gateway makes carries the error's remedy, if it has
  one (`errorResult`, `hinted`).
- The Agent's session is logged and its Audit log started exactly once, guarded
  by `Gateway.agentLogged` (`noteAgent`).

Code mode:

- `execute` is registered only when `Features.CodeMode` is non-nil, and after
  every Namespaced tool, because its description lists them (`addExecute`).
- A Script may call only tools whose Upstream marks them `readOnlyHint: true`.
  Any other name is an error telling the Agent to call the tool directly
  (`scriptRun.call`).
- A Script's call takes the same path as a direct call, so it is classified,
  logged and audited the same way (`namespacedTool.call`).
- Limits per Script: `CodeMode.ScriptTimeLimit` of wall time, 64 MB of memory,
  5000 stack slots, 40000 characters of output, 8 calls at once and 100 calls in
  all (`CodeMode.limits`).
- A cancelled Script ends in `ctx`'s error; any other failure is an `isError`
  result (`runScript`).

stdio Upstreams:

- The child's environment is `StartOptions.Env` followed by the Upstream's
  Config env, so the Config wins on a duplicated key (`upstreamEnv`).
- The state machine has one path each way (`next`): `connecting` to `serving`
  on tools listed; `connecting` to `quit` or `serving` to `exited` when the
  Upstream ends the session; `connecting` or `serving` to `stopping` when the
  Gateway closes it; `quit` to `exited` if the tools turn out to be listed.
  State is read and written only under `stdioUpstream.mu`.
- An Upstream's exit is logged only if it ended the session after its tools
  were listed, and only once (`stdioUpstream.report`).
- `Close` always follows one sequence: close stdin, SIGTERM the process group,
  wait up to `stopGrace` (5s), SIGKILL if anything is left and wait up to one
  more second for the group to be gone, then wait up to `exitGrace` (2s) for
  the process to be reaped before giving up on it.

HTTP and secrets:

- An Upstream is an OAuth Upstream only if it is HTTP, its Config sets no
  `Authorization` header, and its URL is HTTPS or plain HTTP on a loopback
  address (`checkOAuth`, `isSecure`). This is decided once, in `Auths.For`.
- Config headers and the bearer token go only to the Upstream's own origin:
  same scheme, host and port (`sameOrigin`, `headerTransport`,
  `bearerTransport`).
- An OAuth Upstream's client refuses a redirect to another origin
  (`sameOriginRedirects`); a request stops at `maxRedirects` (10).
- Round-trippers clone a request before changing it (`headerTransport`,
  `bearerTransport.send`, `withResource`).
- An HTTP Upstream's proxy comes from the `env` given to `OpenAuths`, not from
  `StartOptions.Env` and not from the process environment (`Auth.transport`,
  `newHTTPTransport`, `proxyConfig`).
- The deprecated HTTP+SSE transport is not supported; a server that speaks only
  it fails with an error that says so (`dialHTTP`, `sseOnly`).

OAuth in serve:

- Serve never starts a Login. The token is sent and renewed in the Upstream's
  own transport, not through the SDK's `TokenSource` (`storedCredentials`).
- On a 401 the token is renewed and the request resent at most once, and only
  if the request can be resent (`bearerTransport.RoundTrip`).
- Before renewing, the Upstream must still name the Authorization Server that
  issued the Credentials; otherwise the refresh token is not sent and a Login
  is needed (`renewToken`, `storedCredentials.issuer`, `sameIssuer`).
- An error that holds a `*credentials.LoginNeededError` is returned as a
  `needsLoginError` carrying `Auth.Remedy` as its hint (`storedCredentials.fail`).
  `check` reports it as `HealthNeedsLogin`.
- Error responses from an Authorization Server are reduced to their OAuth error
  code before the SDK or `oauth2` can quote them (`errorBodies`, `oauthError`).
- `Auth.Status` contacts no server, so Credentials that the Authorization Server
  would reject read as good (`AuthStatus`).

Login:

- `Login` fails first with why the Upstream is not an OAuth Upstream, or why the
  Store can't be located (`Auth.store`).
- The Config's client is used if it names one; otherwise one is registered with
  Dynamic Client Registration, and only that one is stored in the Credentials
  (`preregisteredClient`, `Auth.Login`).
- A new Login requests the scopes the stored Credentials already had, on top of
  the Config's or the discovered ones (`ScopeFilter` in `Auth.Login`, `union`).
- The callback server accepts exactly one callback: the first `GET /callback`
  whose `state` matches. `state` is checked before anything else is read
  (`callbackServer.ServeHTTP`).
- An `iss` that differs from the expected issuer, or a missing `iss` when the
  server's metadata says it sends one, rejects the response (RFC 9207).
- The Login runs at most once per call (`authorizeOnce`) and waits up to
  `callbackTimeout` (5 minutes) for the callback.
- The browser is opened only for an HTTPS or loopback URL, as one argument to
  the opener, never through a shell (`openBrowser`).
- DEBUG logs of a Login never hold the request URL's query string or userinfo
  (`logURL`), a token request's body, or the top-level fields in `secretFields`
  of a logged JSON body (`exchangeLog`, `redactJSON`). The redaction goes no
  further than that: a redirect's `Location` is cut only at its `?`, the
  `WWW-Authenticate` header is logged as it is, and `redactJSON` does not look
  inside nested objects.

## Side Effects

- **Child processes**: each stdio Upstream is launched with `exec.Command` in
  its own process group (`Setpgid`), and signalled as a group with `SIGTERM`
  and `SIGKILL` (`stdioUpstream.Connect`, `Close`, `signal`). `Check` and
  `Reports` launch and stop real Upstreams too.
- **Browser**: `Auth.Login` with `browser` set runs `open` on darwin or
  `xdg-open` on linux (`openBrowser`).
- **Network, Upstreams**: Streamable HTTP connections to each HTTP Upstream, the
  MCP handshake and tool listing at startup, and every proxied tool call. A
  failed connect is followed by one HTTP+SSE probe of at most 2 seconds
  (`sseOnly`), unless the OAuth handler recorded an error such as a needed
  Login, which is returned first (`dialHTTP`).
- **Network, Authorization Servers**: discovery, client registration and the
  code exchange at Login; Protected Resource Metadata lookups and a
  refresh-token request at renewal. Each request is bounded by `oauthTimeout`
  (30s) (`oauthClient`).
- **Listening socket**: `Auth.Login` listens on `127.0.0.1` at a port the OS
  picks, serves `/callback`, and stops after one accepted callback or when
  `Login` returns (`listenForCallback`, `callbackServer.Close`).
- **Credentials Store**, through `internal/credentials`: `OpenAuths` opens it,
  `Status` and `Login` load from it, `Login` saves to it, `Forget` and `Logout`
  delete from it, and serve renews through `store.Source`. The Store's locking
  is described in `docs/adr/0003-credentials-in-locked-files.md`.
- **Environment**: the package reads no process environment itself; it takes
  two slices. The `env` given to `OpenAuths` locates the Credentials Store
  (`XDG_STATE_HOME`, `HOME`), gives the proxy of HTTP Upstreams (`HTTP_PROXY`,
  `HTTPS_PROXY`, `NO_PROXY` and their lower-case forms, `proxyConfig`) and is
  the environment of the browser opener. `StartOptions.Env`, followed by the
  Config env, is the environment of stdio Upstream processes (`upstreamEnv`).
  The package does not require the two to be the same slice; `internal/cli`
  passes the same one. A nil environment for a child process makes `os/exec`
  give it the process environment.
- **Streams**: `Serve` reads MCP from the reader and writes it to the writer it
  is given. `Login` writes the issuer, scopes and authorization URL to
  `prompt`. The package itself never touches `os.Stdin` or `os.Stdout`.
- **Logging** through the `slog.Logger` it is given: INFO `upstream ready`,
  `gateway started`, `agent connected`, `upstream stderr` (one record per
  stderr line of a stdio Upstream; a line of `maxStderrLine` or more is logged
  in pieces) and `login complete`; WARN
  `upstream failed; skipped`, `tool skipped`, `upstream exited` and
  `upstream did not stop`; DEBUG `tool added`, `tool called`,
  `script executed`, `credentials renewed` and `http exchange`.
- **Audit log**, when `Features.Audit` is set: one session record when the Agent
  connects, one call record per Upstream call, and one Script record per
  `execute` call, which holds the Script's source (`noteAgent`,
  `namespacedTool.call`, `Gateway.execute`).
- **Sandbox**: each `execute` call runs its Script through `sandbox.Run`
  (`runScript`).
- **Goroutines**: one per Upstream start and one per connect (waited for by
  `upstreams.close`), one reaper per stdio Upstream, one per session on close,
  and during a Login one for the callback server and one to reap the opener.

## Verification

- Run: `go test ./internal/gateway/...`
