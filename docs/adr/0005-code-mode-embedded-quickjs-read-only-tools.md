# Code mode: embedded QuickJS, read-only tools only

With `sprut serve --code-mode` the Gateway adds one tool, `execute`, next to the Namespaced tools. It runs a Script, a JavaScript program the Agent writes, in a QuickJS VM embedded in the `sprut` process (`modernc.org/quickjs`, cgo-free). The Script can call Namespaced tools through `call(name, args)`, but only those whose Upstream marks them `readOnlyHint: true`. The point is composition: loops, filters and joins across Upstreams whose intermediate results never pass through the Agent's context. Saving on tool definitions is not the point, because the Agents' own tool search already defers them (`docs/research/code-mode.md` §7). Restricting Scripts to read-only tools keeps every write a direct call that the Agent's per-tool approval rules still see. That keeps `execute` itself honestly read-only, so an Agent can approve it once without approving every write in every Upstream. Code mode is opt-in per Agent, so an Agent with a client-side code mode of its own (Codex) does not nest one inside the other. It stays MCP-only (ADR-0002) and in-process (ADR-0001).

## Considered Options

- **No code mode; leave composition to client-side code mode (Codex `exec`)**: no new surface in sprut, and approvals survive natively. But only one Agent has it, and it is unreleased.
- **Replace the Namespaced tools with `search` + `execute`** (Cloudflare, gridctl): smallest tool list. But every write would go through `execute`, and allowing it in the Agent would allow every write.
- **Let Scripts call every tool, and mark `execute` `destructiveHint: true`**: works for Upstreams that don't annotate their tools. But approval of a single Script becomes blanket approval of every write, which the MCP client best-practices doc warns against. `readOnlyHint` is an untrusted hint, but an Upstream that lies about it only hurts itself.
- **goja**: the most-used Go engine, with Go precedent (Docker MCP Gateway). But it has no memory limit, so a Script that runs out of memory kills the process and every stdio Upstream of that Agent. It would also need esbuild (+10.4 MB together), or a child process with rlimits that hold only on Linux.
- **QuickJS on wazero** (fastschema/qjs): hard memory isolation, but its defaults are unsafe (CWD mounted, stdout shared with the MCP stream), and its timeout is a no-op.
- **v8go**: needs cgo, which breaks the static release.
- **TypeScript through esbuild**: +3.7 MB for syntax that Cloudflare and Codex both tell the model not to use. Tool signatures are shown in TS notation as documentation only.
