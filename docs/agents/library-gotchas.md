# Library gotchas

Behaviour of go-sdk and urfave/cli that no signature reveals. For the API itself, prefer `go doc` to reading source. Each item names the version it was verified against. When `go.mod` moves past that version, re-verify the item before relying on it, and then update or delete it.

When you do need the source, find it with `go list -m -f '{{.Dir}}' <module>`. The module cache on this machine is not under `~/go`.

## go-sdk (`github.com/modelcontextprotocol/go-sdk`)

- **Results are stamped per hop** (v1.8.0). Under the modern protocol, the server sets `_meta["io.modelcontextprotocol/serverInfo"]` on each result *unless one is already present*, and it adds `resultType`. Before forwarding an Upstream's result, remove that `_meta` key. Otherwise the Upstream's server info goes out in place of sprut's.
- **`Server.AddTool` panics** on a tool whose input schema is missing or is not `type: object` (v1.8.0). It panics before adding anything, so the Gateway recovers around it and skips the tool.
- **`Server.Run` ends the session on stdin EOF** and drops responses that are still in flight (v1.8.0).
- **`CommandTransport.Close`** closes the child's stdin, waits (5s by default), and then sends SIGTERM to that one process (v1.8.0). It does not use process groups, so the child's own children can outlive it. This is why sprut runs stdio Upstreams through its own transport (`gateway.stdioTransport`) instead.
- **A failed `Client.Connect` closes the session before returning** (v1.8.0). Over a stdio transport that means stopping the child with its grace period (the `CommandTransport.Close` above, or sprut's `stdioTransport`), so a deadline on `Connect`'s ctx does not bound how long `Connect` takes to return when the child is slow to exit. To enforce a deadline, stop waiting on `Connect` at the deadline and let it finish in the background, as `gateway.starter.start` does.
- **`Connect`'s ctx bounds only the handshake** (v1.8.0). The connection detaches from its cancellation, so a short-lived startup ctx is safe: cancelling it later leaves the session open.
- **`Server.AddReceivingMiddleware` can rewrite any outgoing result** (v1.8.0). Use it in a fake Upstream to serve what the SDK's own API refuses to register, such as a tool whose input schema is not an object.

## urfave/cli (`github.com/urfave/cli/v3`)

- **Exits and stderr go to globals** (v3.14.0). Unless the root command sets `ExitErrHandler`, an error carrying an exit code goes to the package-global `OsExiter` (`os.Exit`) and the package-global `ErrWriter`. Setting the root command's own `ErrWriter` does not reroute this path; only `ExitErrHandler` does.
- **Root help always goes to the root command's `Writer`** (v3.14.0).
- **Usage errors print help to stdout** (v3.14.0), not to stderr, and exit 1. This covers flag parse errors and errors from a flag's `Validator`. Setting `OnUsageError` on a command replaces both the help dump and the "Incorrect Usage" line for that command's flags; return `ucli.Exit(err.Error(), 2)` from it.
