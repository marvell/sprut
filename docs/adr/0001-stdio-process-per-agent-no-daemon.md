# stdio process per Agent, no daemon

Each Agent launches its own `sprut` as a stdio MCP server, and that process starts every Upstream itself. We accept that several open Agents run duplicate copies of each Upstream in exchange for having no daemon, no port, no service manager and no shared state. Every Agent supports stdio, which keeps setup to a single line per Agent.

## Considered Options

- **Shared daemon over Streamable HTTP**: one copy of each Upstream and shared auth sessions, but requires launchd/systemd, a listening port and lifecycle management. Deferred until duplication becomes a real problem.
