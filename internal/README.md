# internal

## Overview

Every package of sprut besides `main`. The directory holds no source files of its own. `main` imports only `cli`; the packages import each other as follows (test files excluded):

| Package | Imports |
| --- | --- |
| `cli` | `audit`, `config`, `gateway` |
| `gateway` | `audit`, `config`, `credentials`, `sandbox` |
| `credentials` | `atomicfile`, `config`, `statedir` |
| `audit` | `chars`, `config`, `statedir` |
| `config` | `atomicfile` |
| `statedir` | `config` |
| `sandbox` | `chars` |
| `atomicfile`, `chars`, `mcptest` | none in this module |

`mcptest` is imported only by test files: the root `main_test.go`, and the tests of `cli` and `gateway`.

## Subdirectories & Boundaries

- cli: [./cli/README.md](./cli/README.md) - The whole program as a function: `cli.Run` takes args, env and the standard streams and returns the exit code. It owns the command tree (`serve`, `upstream add|remove|enable|disable|list|login|logout`), usage errors, exit codes and the text on stdout and stderr.
- gateway: [./gateway/README.md](./gateway/README.md) - Connects to the Upstreams and serves their tools to one Agent as Namespaced tools `<upstream>__<tool>` over a single MCP server: `Start`, `Serve`, `Close`, the stdio and Streamable HTTP transports, OAuth (`Auths`, `Auth.Login`), the diagnostics `Check` and `Reports`, and the `execute` tool of Code mode.
- config: [./config/README.md](./config/README.md) - Turns a Config file (JSON with an `mcpServers` object) into Upstream definitions (`Parse`, `ParseEntries`, `Open`) and edits it in place while keeping order and unknown keys (`Document`, `Edit`). Reads the environment only through a caller-supplied `LookupEnv`.
- credentials: [./credentials/README.md](./credentials/README.md) - Stores each OAuth Upstream's Credentials in a file of its own, written only under a flock on a sibling lock file (`Store`), and renews them once at a time in a process and across processes (`Source`). Makes no network call itself: the caller supplies the `Refresh`.
- sandbox: [./sandbox/README.md](./sandbox/README.md) - Runs a Script in a fresh QuickJS VM that offers only `console` and `call` (`Run`, `Caller`, `Limits`). Knows nothing of MCP or Upstreams.
- audit: [./audit/README.md](./audit/README.md) - Writes the Audit log: one JSONL file per `sprut serve` with the session, every Namespaced tool call that reaches an Upstream and every Script (`Log`, `New`, `Start`, `Call`, `Script`, `Close`). `Start`, `Call` and `Script` never fail their caller.
- statedir: [./statedir/README.md](./statedir/README.md) - Locates sprut's state directory (`Dir`) and makes directories under it private, mode 0700 (`MkdirPrivate`).
- atomicfile: [./atomicfile/README.md](./atomicfile/README.md) - Replaces a file through a synced temporary file renamed over it (`Write`), so a reader sees the old file whole or the new one whole.
- chars: [./chars/README.md](./chars/README.md) - Cuts text to a number of characters, not bytes (`Cut`).
- mcptest: [./mcptest/README.md](./mcptest/README.md) - Fake Upstreams shared by the tests: a stdio fake that is the test binary re-executing itself (`Stdio`, `MainIfFake`, `Main`), HTTP fakes on httptest servers (`StartHTTP` and others), and an OAuth Upstream with its Authorization Server (`StartOAuth`).
