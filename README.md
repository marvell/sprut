# sprut

sprut is an MCP Gateway. You list your MCP servers once, in one Config file, and give each AI Agent (Claude Code, Codex, Cursor, …) a single MCP server entry: `sprut serve`. Every Agent then sees all of your MCP servers' tools.

- One Config in the `mcpServers` JSON shape that MCP READMEs already show, so you can paste their snippets unchanged.
- Tools are exposed as `<upstream>__<tool>`, so tools from different Upstreams never collide.
- An Upstream that fails to start is skipped with a log line; the rest keep working.
- stdio and Streamable HTTP Upstreams. Legacy and modern (`2026-07-28`) MCP Agents and Upstreams work in any combination.
- One static binary. Each Agent runs its own `sprut serve` over stdio, with no daemon.

## Install

From source (Go 1.27 or later):

```sh
go install github.com/marvell/sprut@latest
```

Or download a prebuilt binary for linux or darwin (amd64, arm64) from [GitHub Releases](https://github.com/marvell/sprut/releases) and put `sprut` on your `PATH`.

Check it:

```sh
sprut --version
```

## Config

sprut reads the first of these that is set:

1. `sprut serve -c PATH` (or `--config PATH`);
2. `$SPRUT_CONFIG`;
3. `$XDG_CONFIG_HOME/sprut/config.json`;
4. `~/.config/sprut/config.json`.

```json
{
  "mcpServers": {
    "memory": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-memory"]
    },
    "github": {
      "url": "https://api.githubcopilot.com/mcp/",
      "headers": { "Authorization": "Bearer ${GITHUB_TOKEN}" }
    },
    "postgres": {
      "command": "uvx",
      "args": ["postgres-mcp"],
      "env": { "DATABASE_URI": "${DATABASE_URI}" },
      "disabled": true
    }
  }
}
```

Each key under `mcpServers` names an Upstream. It must match `^[a-z0-9-]+$`, and it becomes the prefix of that Upstream's tools (`memory__read_graph`).

| Field | Meaning |
| --- | --- |
| `command`, `args`, `env` | A stdio Upstream. It runs with sprut's environment plus `env`. |
| `url`, `headers` | A Streamable HTTP Upstream, with static headers. |
| `type` | Optional: `stdio` or `http`. Inferred from `command` or `url` when absent. `sse` is not supported, and such an Upstream is skipped. |
| `disabled` | `true` turns the Upstream off without deleting it. |

`${VAR}` in `args`, `env`, `url` and `headers` is replaced from sprut's environment, so secrets stay out of the file. If a variable is unset, sprut skips that Upstream with a warning instead of substituting an empty string. Any other field is ignored with a warning.

## Add sprut to your Agents

Each Agent runs `sprut serve` as a stdio MCP server. To give one Agent a different set of Upstreams, add `-c PATH` to its `args`.

**Claude Code**

```sh
claude mcp add --scope user sprut -- sprut serve
```

**Codex** (`~/.codex/config.toml`)

```toml
[mcp_servers.sprut]
command = "sprut"
args = ["serve"]
```

**Cursor** (`~/.cursor/mcp.json`), **Gemini CLI** (`~/.gemini/settings.json`), **Claude Desktop** (`claude_desktop_config.json`), and others using the same shape:

```json
{
  "mcpServers": {
    "sprut": {
      "command": "sprut",
      "args": ["serve"]
    }
  }
}
```

**VS Code** (`.vscode/mcp.json`)

```json
{
  "servers": {
    "sprut": {
      "type": "stdio",
      "command": "sprut",
      "args": ["serve"]
    }
  }
}
```

If the Agent does not find `sprut` on its `PATH`, use the binary's absolute path as the command.

## Check your Config

`--dry-run` starts every Upstream exactly as a real run would and logs the same lines, then shuts them down. It exits 0 if every enabled Upstream started, and 1 if any failed:

```console
$ sprut serve --dry-run
time=... level=WARN msg="unset variable GITHUB_TOKEN; upstream skipped" upstream=github
time=... level=INFO msg="upstream ready" upstream=memory tools=9 protocol=2025-06-18 duration=812ms
time=... level=INFO msg="gateway started" ready=1 failed=0 tools=9
```

Logs always go to stderr in logfmt, and stdout carries only MCP traffic. `-v` adds DEBUG lines: every Namespaced tool and every tool call. Each Upstream gets `--startup-timeout` (default `30s`) to start.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Clean exit: the Agent closed stdin, SIGINT or SIGTERM, or a successful dry run. |
| 1 | A Config error, a session with the Agent that fails other than by EOF on stdin (such as a read error), or a dry run in which some Upstream failed. |
| 2 | A usage error, such as an unknown flag or command. Bare `sprut` exits 2 too, so it never starts the server by accident. |

## License

[MIT](LICENSE)
