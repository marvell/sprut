# Sprut

Sprut is an MCP gateway: a single MCP server that gives any AI agent access to all of the user's MCP servers at once, so each agent doesn't have to be configured server by server.

## Language

**Gateway**:
Sprut itself, as seen by an Agent: one MCP server standing in for all Upstreams.
_Avoid_: proxy, aggregator, hub

**Upstream**:
One of the user's MCP servers that sits behind the Gateway, declared in the Config under a unique name.
_Avoid_: backend, child server, MCP server (ambiguous)

**Agent**:
An AI client (Claude Code, Codex, Cursor, …) that connects to the Gateway.
_Avoid_: client, host

**Config**:
The single file that lists the Upstreams, in the de-facto `mcpServers` JSON shape.
_Avoid_: settings, manifest

**Namespaced tool**:
A tool as the Agent sees it: the Upstream's tool name prefixed with the Upstream's name, so tools from different Upstreams never collide.
_Avoid_: aliased tool, prefixed tool
