# Library gotchas

Behaviour of go-sdk, urfave/cli and os/exec that no signature reveals. For the API itself, prefer `go doc` to reading source. Each item names the version it was verified against. When `go.mod` moves past that version, re-verify the item before relying on it, and then update or delete it.

When you do need the source, find it with `go list -m -f '{{.Dir}}' <module>`. The module cache on this machine is not under `~/go`.

## go-sdk (`github.com/modelcontextprotocol/go-sdk`)

- **Results are stamped per hop** (v1.8.0). Under the modern protocol, the server sets `_meta["io.modelcontextprotocol/serverInfo"]` on each result *unless one is already present*, and it adds `resultType`. Before forwarding an Upstream's result, remove that `_meta` key. Otherwise the Upstream's server info goes out in place of sprut's.
- **`Server.AddTool` panics** on a tool whose input schema is missing or is not `type: object` (v1.8.0). It panics before adding anything, so the Gateway recovers around it and skips the tool.
- **`Server.Run` ends the session on stdin EOF** and drops responses that are still in flight (v1.8.0).
- **`CommandTransport.Close`** closes the child's stdin, waits (5s by default), and then sends SIGTERM to that one process (v1.8.0). It does not use process groups, so the child's own children can outlive it. This is why sprut runs stdio Upstreams through its own transport (`gateway.stdioTransport`) instead.
- **A failed `Client.Connect` closes the session before returning** (v1.8.0). Over a stdio transport that means stopping the child with its grace period (the `CommandTransport.Close` above, or sprut's `stdioTransport`), so a deadline on `Connect`'s ctx does not bound how long `Connect` takes to return when the child is slow to exit. To enforce a deadline, stop waiting on `Connect` at the deadline and let it finish in the background, as `gateway.starter.start` does.
- **`Connect`'s ctx bounds only the handshake** (v1.8.0). The connection detaches from its cancellation, so a short-lived startup ctx is safe: cancelling it later leaves the session open.
- **A call's error is a `*jsonrpc.Error` only when the peer answered with one** (v1.8.0). Connection loss never matches `errors.As` with a `*jsonrpc.Error` target: `ClientSession.CallTool` wraps the closing errors (themselves wire errors) with `%v`, not `%w`. Do not test for `mcp.ErrConnectionClosed` to detect a dead peer, either. A call already in flight when the peer dies gets the raw read error (`io.EOF`), and only calls made after that wrap `ErrConnectionClosed`. `gateway.forward` relies on both.
- **A raw `ToolHandler`'s error becomes a protocol error** (v1.8.0), not an `isError` result. An unknown tool name is a protocol error too (`-32602`, "unknown tool").
- **A client retries multi-round-trip calls itself by default** (v1.8.0). When a modern server answers a call with `inputRequests`, the client's middleware fulfils them with its own handlers and calls again; with no handler, the call fails with a wire error. `ClientOptions.MultiRoundTrip.Disabled` returns the result instead, and `CallToolResult.NeedsInput` reports it. The Gateway does this and answers with an `isError` result.
- **A server learns a modern client's protocol version from its first request** (v1.8.0), not from a handshake: `ServerSession.InitializeParams` stays nil until then. For a legacy client, `InitializeParams.ProtocolVersion` is the version the client asked for; the negotiated one is only in the `InitializeResult`.
- **`StreamableClientTransport` never falls back to HTTP+SSE** (v1.8.0); that transport is the separate `SSEClientTransport`. An SSE-only server answers the initialize POST with 400, which surfaces only as `calling "initialize": sending "initialize": Bad Request`. So after a failed connect, `gateway.sseOnly` probes with a GET, which only an HTTP+SSE server answers with an event stream, and names the reason.
- **Only a stateless `StreamableHTTPHandler` serves the modern protocol** (v1.8.0). A stateful one drops versions from 2026-07-28 on, and a modern-only stateful server then fails every client. A fake modern HTTP Upstream needs `StreamableHTTPOptions.Stateless`.
- **A Streamable HTTP request outlives the call that sent it** (v1.8.0). Each POST stays open until the server answers. The caller's ctx and closing the session do not end it. When `Connect` fails on its deadline, it closes the session. That close waits up to 5s (`notifyCancellationTimeout`) for the `notifications/cancelled` POST. A hung HTTP Upstream therefore holds up `Gateway.Close` by up to 5s, and a test server that never answers must be released before `httptest.Server.Close`.
- **`Server.AddReceivingMiddleware` can rewrite any outgoing result** (v1.8.0). Use it in a fake Upstream to serve what the SDK's own API refuses to register, such as a tool whose input schema is not an object.

## urfave/cli (`github.com/urfave/cli/v3`)

- **Exits and stderr go to globals** (v3.14.0). Unless the root command sets `ExitErrHandler`, an error carrying an exit code goes to the package-global `OsExiter` (`os.Exit`) and the package-global `ErrWriter`. Setting the root command's own `ErrWriter` does not reroute this path; only `ExitErrHandler` does.
- **Root help always goes to the root command's `Writer`** (v3.14.0).
- **Usage errors print help to stdout** (v3.14.0), not to stderr, and exit 1. This covers flag parse errors and errors from a flag's `Validator`. Setting `OnUsageError` on a command replaces both the help dump and the "Incorrect Usage" line for that command's flags; return `ucli.Exit(err.Error(), 2)` from it.

## os/exec and process groups

- **`Wait` closes the pipes from `StdinPipe`/`StdoutPipe`/`StderrPipe`** (go1.27.1), even while they are still being read. To reap a child in the background while reading its output, give it an `os.Pipe` write end instead, as `gateway.stdioTransport` does for stdout.
- **`Wait` waits for exec's copy goroutines** (go1.27.1) when `Stdout`/`Stderr` is a writer that is not an `*os.File`. `WaitDelay` bounds that wait after the child exits, then closes the pipes, and `Wait` still waits for the goroutines to finish. So once `Wait` has returned, nothing writes to that writer any more.
- **An unreaped leader still counts as a member of its process group** (darwin and linux). `kill(-pgid, 0)` returns ESRCH only once the leader is reaped and every other member is gone.
- **A stdio Upstream's environment is only what `cli.Run` received** (go1.27.1). It has no `PATH` unless the caller passed one, so a fake Upstream in a test starts helper programs by absolute path (`/bin/sleep`).
