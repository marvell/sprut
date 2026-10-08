# sprut

sprut is an MCP Gateway. You list your MCP servers once, in one Config file, and give each AI Agent (Claude Code, Codex, Cursor, …) a single MCP server entry: `sprut serve`. Every Agent then sees all of your MCP servers' tools.

- One Config in the `mcpServers` JSON shape that MCP READMEs already show, so you can paste their snippets unchanged.
- Tools are exposed as `<upstream>__<tool>`, so tools from different Upstreams never collide.
- An Upstream that fails to start is skipped with a log line; the rest keep working.
- OAuth for HTTP Upstreams such as Notion, Linear or Atlassian: log in once with `sprut auth login`, and every Agent's sprut keeps the tokens renewed.
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

1. `-c PATH` (or `--config PATH`), which `sprut serve` and every `sprut auth` command take;
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
| `oauth` | Optional, HTTP only: `clientId`, `clientSecret` and `scopes` for [OAuth](#oauth). |
| `type` | Optional: `stdio` or `http`. Inferred from `command` or `url` when absent. `sse` is not supported, and such an Upstream is skipped. |
| `disabled` | `true` turns the Upstream off without deleting it. |

`${VAR}` in `args`, `env`, `url`, `headers` and the `oauth` fields is replaced from sprut's environment, so secrets stay out of the file. If a variable is unset, sprut skips that Upstream with a warning instead of substituting an empty string. Any other field is ignored with a warning.

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

## OAuth

An HTTP Upstream with no `Authorization` header uses OAuth when the server asks for it by answering 401, so a snippet with only a `url` works unchanged:

```json
{
  "mcpServers": {
    "linear": { "url": "https://mcp.linear.app/mcp" }
  }
}
```

OAuth runs only over HTTPS, or plain HTTP to a loopback address (`localhost`, `127.0.0.1`, `::1`).

### Log in

`sprut serve` never opens a browser. Log in once per Upstream:

```console
$ sprut auth login linear
Logging in to linear.
  authorization server: https://...
  scopes: ...

Opening this URL in your browser; if it does not open, open it yourself:

  https://mcp.linear.app/authorize?...
```

sprut prints the authorization server and the scopes it requests before the URL, and waits up to 5 minutes for the browser to come back to it on `http://127.0.0.1:<port>/callback`. Running `sprut serve` processes pick up the new Credentials at their next request, without a restart; an Upstream that was skipped at startup appears once the Agent restarts.

`sprut auth login` with no names logs in to every OAuth Upstream in the Config that needs it: those with no Credentials, and those whose Credentials can no longer be renewed. It exits 1 if any Login failed.

**Over ssh**, `--no-browser` only prints the URL. The callback goes to the machine sprut runs on, so forward its port: start the Login, note the port in the URL's `redirect_uri`, and in another terminal run `ssh -L <port>:127.0.0.1:<port> <host>` before opening the URL in your local browser.

### Status and logout

```console
$ sprut auth status
linear: logged in, access token expires 2026-10-08T21:10:00+03:00, renewable
notion: no credentials
$ sprut auth logout linear
linear: logged out
```

`auth status` lists every HTTP Upstream that may use OAuth (no `Authorization` header, HTTPS or loopback), including one whose server never asks for it. It reads only the stored Credentials and contacts no server, so Credentials that the server has revoked still read as logged in until a request finds out. `auth logout` deletes the Credentials; every running `sprut serve` stops sending them at its next request. It does not revoke the tokens at the server.

When Credentials die (the server rejects the refresh token, or there is none and the access token expired), the Upstream's tool calls return an error naming `sprut auth login <upstream>`, and an Upstream skipped at startup for that reason is listed in the Gateway's MCP `instructions`, so the Agent can tell you.

### The `oauth` object

```json
{
  "mcpServers": {
    "corp": {
      "url": "https://mcp.example.com/mcp",
      "oauth": {
        "clientId": "sprut-cli",
        "clientSecret": "${CORP_CLIENT_SECRET}",
        "scopes": ["mcp:read", "mcp:write"]
      }
    }
  }
}
```

| Field | Meaning |
| --- | --- |
| `clientId`, `clientSecret` | A client registered with the authorization server beforehand. Without them, each Login registers a new client with Dynamic Client Registration. |
| `scopes` | Exactly the scopes to request. Without it, sprut requests what the server's challenge names, or else every scope it lists as supported. `[]` requests none. |

Changing `url`, `oauth.clientId` or `oauth.scopes` needs a new Login; changing `oauth.clientSecret` does not.

### Where Credentials live

In `$XDG_STATE_HOME/sprut/credentials/<upstream>.json` (`~/.local/state/sprut/credentials/` by default), readable only by you (file `0600`, directories `0700`). Each has a `<upstream>.lock` beside it, which every sprut process takes before renewing, so that several Agents' sprut processes share one Upstream's Credentials without spending a rotating refresh token twice. Tokens, client secrets and authorization codes never appear in logs, even with `-v`, and errors from an authorization server are reported only by their error code and HTTP status.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Clean exit: the Agent closed stdin, SIGINT or SIGTERM, or a successful dry run. |
| 1 | A Config error, a session with the Agent that fails other than by EOF on stdin (such as a read error), a dry run in which some Upstream failed, or a `sprut auth` command that failed: a Login that failed, or an Upstream that is not in the Config or not an OAuth Upstream. |
| 2 | A usage error, such as an unknown flag or command. Bare `sprut` exits 2 too, so it never starts the server by accident. |

## License

[MIT](LICENSE)
