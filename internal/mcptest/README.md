# mcptest

## Overview

Package `mcptest` holds the fake Upstreams that the repository's tests share.
It is ordinary (non-`_test.go`) source so that any test package can import it:
the root `main_test.go` (testscript), the `internal/cli` scenarios and the
`internal/gateway` tests.

It provides three kinds of fake:

- **stdio Upstream** (`stdio.go`): the test binary re-executing itself as an
  MCP server, with its behaviour (and misbehaviour) described by a `Stdio`
  spec carried as JSON in the `SPRUT_TEST_FAKE_UPSTREAM` environment variable.
- **HTTP Upstreams** (`http.go`): `httptest` servers speaking Streamable HTTP,
  the deprecated HTTP+SSE transport, or deliberately broken variants.
- **OAuth Upstream** (`oauth.go`): an MCP endpoint and its Authorization
  Server on one `httptest` server, with knobs for every failure a Login or a
  renewal has to survive.

Every fake MCP server is built by `NewServer` and offers the tools `echo`
(read-only) and `fail` (not annotated), plus whatever its spec adds.

## Interface

### Shared server and tool fixtures (`mcptest.go`)

- `Legacy = "2025-06-18"`, `Modern = "2026-07-28"`: the protocol versions of
  the two eras.
- `NewServer(protocol string) *mcp.Server`: the MCP server of a fake Upstream
  (implementation name `fake-upstream`, version `0.0.1`) with `echo` and
  `fail`. A non-empty `protocol` becomes its only supported protocol version.
- `ObjectSchema`, `EchoInputSchema`, `EchoOutputSchema`: the tools' schemas as
  raw `map[string]any`, for asserting that the Gateway passes them through
  unchanged.
- `EchoResult(text string) map[string]any`: the JSON a fake answers `echo`
  with: text content `"echo: " + text`, `structuredContent.echoed`, and
  `_meta["fake/trace"] = "abc"`.

### stdio Upstream (`stdio.go`)

- `Stdio`: the spec of a fake stdio Upstream; the zero value is well-behaved.
  - Era and startup: `Protocol`, `Startup` (`Fail`, `Hang`), `Stderr`,
    `Rendezvous`.
  - Files written for the test to read back: `PIDFile`, `EnvFile`,
    `GrandchildPIDFile`, `LeaveGroupPIDFile`, `OrphanToolPIDFile`,
    `BlockToolFile`.
  - Process behaviour: `IgnoreSIGTERM`, `HangUpAfterList`.
  - Extra tools: `ExtraTools` with `ID` (each answers with `ID`),
    `BadSchemaTool`, `CrashTool` (`crash`), `OrphanToolPIDFile` (`orphan`),
    `BlockToolFile` (`block`), `GatherTool` (`gather`), `AskTool` (`ask`),
    `ReplyTool` (`reply`), `WriteTool` (`write`).
- `Startup` with constants `Fail` (exits 1 before speaking MCP) and `Hang`
  (never answers, ignores stdin closing).
- `Rendezvous{N, Dir}`: makes a fake hold back until `N` fakes have marked
  their arrival in `Dir`.
- `Stdio.Entry(t testing.TB, env ...string) map[string]any`: a Config entry
  (`command`, `args`, `env`) that launches the fake as the current test
  binary with `-test.run=^$`.
- `MainIfFake()`: runs the fake and exits if `SPRUT_TEST_FAKE_UPSTREAM` is
  set; otherwise returns.
- `Main()`: runs the fake described by `SPRUT_TEST_FAKE_UPSTREAM`, or a
  well-behaved one if it is unset or empty, and exits. Registered as the
  `fake-upstream` command of the testscript suite.
- `Lifetime = time.Minute`: the upper bound on how long a fake, or a child
  that only waits to be stopped, stays alive.

### HTTP Upstreams (`http.go`)

- `StartHTTP(t testing.TB, protocol string) *HTTPUpstream`: go-sdk's
  Streamable HTTP handler serving `NewServer(protocol)`.
- `HTTPUpstream`: fields `URL` and `Close` (stops the server); method
  `Headers() []http.Header` returns the headers of every request so far.
- `StartSSEOnly(t testing.TB) string`: an Upstream that speaks only the
  deprecated HTTP+SSE transport; returns its URL.
- `StartGETStream(t testing.TB, chunk string) string`: answers every non-GET
  with 500 and a GET with a `text/event-stream` that repeats `chunk` until the
  request ends (silent if `chunk` is empty).
- `StartHanging(t testing.TB) string`: leaves its first request unanswered
  until the test ends; answers later requests 202 at once.
- `RefusingURL() string`: the URL of a server that is already closed, so
  connecting is refused.

### OAuth Upstream (`oauth.go`)

- Secrets the fake hands out: `ClientID`, `ClientSecret`, `Code`,
  `AccessToken`, `RefreshToken`, `Scope` (`mcp:tools`), and `Secrets`
  (`ClientSecret`, `Code`, `AccessToken`, `RefreshToken`) for leak checks.
- `StartOAuth(t testing.TB) *OAuthUpstream` and
  `StartOAuthOn(t testing.TB, host string) *OAuthUpstream` (same, with `host`
  in `Base` in place of `127.0.0.1`).
- `OAuthUpstream` fields: `Base` (the origin, also the issuer) and `URL` (the
  MCP endpoint, `/mcp`).
- Endpoints served:
  `GET /.well-known/oauth-protected-resource/mcp`,
  `GET /.well-known/oauth-authorization-server` (and the trailing-slash path,
  which redirects 307 to it), `POST /register`, `GET /authorize`,
  `POST /token`, `/mcp` and `/mcp-moved`.
- Token control: `Issue() map[string]any`, `IssueNoRefreshToken()`,
  `ExpireIn(seconds int)`, `RevokeAccessTokens()`,
  `AcceptClientSecret(secret string)`.
- Failure injection: `FailRefresh(how string)`, `FailRegistration(how string)`,
  `FailCodeExchange(how string)`, `OnRefresh(f func())`, `Release()`,
  `Released() <-chan struct{}`.
- Metadata and routing knobs: `NameIssuer(issuer string)`, `SlashIssuer()`,
  `AdvertiseIss()`, `AdvertiseOfflineAccess()`,
  `OfferScopes(challenge string, supported []string)`,
  `RequireScope(scope string)`, `RedirectWith(params url.Values)`,
  `RedirectMCP(link string)`.
- Observations: `Refreshes() []url.Values`, `Rejected() int`,
  `MCPAuth() []string`, `GrantTypes() []string`, `Registrations() int`,
  `RequestedScope() (string, bool)`, `AppType() string`.

## Invariants

- A test binary that uses `Stdio.Entry` must call `MainIfFake` first thing in
  its `TestMain`; `Entry` launches `os.Executable()` and relies on that call
  to turn the child into the fake instead of running the tests.
- The `Stdio` spec crosses the process boundary as JSON
  (`json.Marshal` in `Entry`, `json.Unmarshal` in `Main`), so every behaviour
  must be an exported, JSON-encodable field. A testscript Config can write the
  same JSON by hand.
- `env` pairs passed to `Entry` are applied last and so override
  `SPRUT_TEST_FAKE_UPSTREAM` and `GOCOVERDIR`.
- `Stdio.prepare` runs before the fake speaks MCP, in this order: `PIDFile`,
  `LeaveGroupPIDFile`, `EnvFile`, `GrandchildPIDFile`, `Stderr`, `Rendezvous`;
  `Startup` is acted on only after it.
- `Rendezvous.meet` counts every entry in `Dir` and polls every 10 ms with no
  timeout of its own: `Dir` must start empty, and a fake waits forever if
  fewer than `N` arrive.
- `gather` counts arrivals and never decrements: it releases all calls when
  exactly `GatherTool` have arrived, and a cancelled call still counts.
- `Lifetime` bounds every wait whose only purpose is to be stopped: `Hang`,
  the deferred sleeps of `IgnoreSIGTERM` and `LeaveGroupPIDFile`, the `crash`
  tool's wait for SIGTERM, and the `/bin/sleep` children.
- The stdio fake needs a Unix-like OS: `startSleeper` runs `/bin/sleep` by
  absolute path (the fake's environment has no `PATH`) and `prepare` calls
  `syscall.Setpgid`.
- `StartHTTP` serves a stateless handler only when `protocol >= Modern`
  (string comparison); a legacy or empty protocol gets a stateful one.
- Every HTTP fake is plain HTTP on an `httptest` server. The Streamable HTTP
  handlers of `StartHTTP` and `StartOAuthOn` set `DisableLocalhostProtection`
  so that they accept requests for another host (through a proxy);
  `StartSSEOnly` passes no options to `mcp.NewSSEHandler`, so it does not.
- `HTTPUpstream` and `OAuthUpstream` guard their state with a mutex, and
  their observation methods return clones (`slices.Clone`), so tests may call
  them while requests are in flight.
- `OAuthUpstream` honours only the refresh token it issued last (`Issue`
  overwrites `refresh`), so one fake serves one renewable set of Credentials.
- `Issue` names the first tokens `AccessToken` and `RefreshToken` and appends
  `-N` (the issue count) to later ones; the granted scope is the `scope` of
  the last authorization request.
- `/authorize` accepts only `client_id=ClientID`,
  `code_challenge_method=S256` and a non-empty `code_challenge`, and redirects
  straight to `redirect_uri` with `Code` (or the `RedirectWith` params) and
  the request's `state`.
- The code exchange at `/token` requires `Code`, the accepted client secret
  and a `code_verifier` whose SHA-256 matches the last challenge; a refresh
  requires the current refresh token, `ClientID` and the accepted client
  secret.
- `/mcp` answers 401 with a `WWW-Authenticate: Bearer resource_metadata=...`
  challenge unless the request carries `Bearer` plus a live, unexpired access
  token, and 403 `insufficient_scope` when `RequireScope` names a scope the
  token was not granted. `RedirectMCP` redirects only the path `/mcp`, not
  `/mcp-moved`.
- `FailRefresh` understands `"invalid_grant"`, `"5xx"`, `"text"`, `"drop"`,
  `"hang"` and `""`; `FailRegistration` and `FailCodeExchange` take the
  `writeEcho` modes `"json"` (400), `"5xx"` (502 JSON), `"text"` (502 plain
  text) and `"403"` (403 plain text). Every `writeEcho` failure echoes
  `RefreshToken`, `ClientSecret` and `Code` on purpose (`echoed`), so that a
  test can detect them leaking; `AccessToken` is not echoed.
- `POST /register` reads `grant_types` with unchecked type assertions: a
  registration body without an array of strings there panics the handler.
- `Release` is idempotent, and `StartOAuthOn` and `StartHanging` register
  their release after `ts.Close` with `t.Cleanup`, so that it runs first and
  `Close` does not wait on a hanging request.

## Side Effects

- **Network**: `StartHTTP`, `StartSSEOnly`, `StartGETStream`, `StartHanging`
  and `StartOAuthOn` each open a loopback `httptest` listener and close it
  with `t.Cleanup`; `RefusingURL` opens one and closes it at once.
- **Process exit**: `Main` always calls `os.Exit`, and `MainIfFake` does when
  `SPRUT_TEST_FAKE_UPSTREAM` is set. The `crash` and `orphan` tools and
  `hangUpWriter` (`HangUpAfterList`) call `os.Exit(1)` mid-run.
- **Child processes**: `startSleeper` starts `/bin/sleep` for
  `GrandchildPIDFile`, `LeaveGroupPIDFile` (in a process group of its own) and
  the `orphan` tool (sharing the fake's stdout and stderr).
- **Signals and process groups**: `IgnoreSIGTERM` calls
  `signal.Ignore(syscall.SIGTERM)`; `LeaveGroupPIDFile` moves the fake into
  the sleeper's process group with `syscall.Setpgid`; the `crash` tool closes
  `os.Stdout` and waits for SIGTERM.
- **Disk**: the fake writes `PIDFile`, `EnvFile` (its whole environment),
  `GrandchildPIDFile`, `LeaveGroupPIDFile`, `OrphanToolPIDFile` and
  `BlockToolFile` (`started`, then `cancelled`) with mode `0o644`, and an
  empty file named after its PID in `Rendezvous.Dir`. `Entry` may create a
  `t.TempDir()` for `GOCOVERDIR` when the test binary is built for coverage.
- **Standard streams**: the fake speaks MCP on stdin and stdout, and writes
  `Stdio.Stderr` and `fake upstream: ...` diagnostics to stderr.
- **Environment**: `Main` and `MainIfFake` read `SPRUT_TEST_FAKE_UPSTREAM`.
  `Entry` only writes it into the entry's `env`; the one variable it reads is
  `GOCOVERDIR`, when the test binary is built for coverage.
- **Recorded state**: `HTTPUpstream` keeps the headers of every request, and
  `OAuthUpstream` keeps each `Authorization` header sent to `/mcp` and the
  form of each refresh request, in memory for the life of the test.

## Verification

- Run: `go test ./internal/mcptest/...`
- The package has no test files of its own, so that command only builds it.
  The fakes are exercised by the packages that import them:
  `go test . ./internal/cli/... ./internal/gateway/...`
