# Code Mode for MCP: research for sprut

Researched 2026-11-07, to feed a design grilling session on giving sprut a code-execution mode. In that mode the Gateway would expose a few tools (e.g. `search` + `execute`) instead of every Namespaced tool, and the Agent would write JS/TS that calls Upstream tools inside a sandbox embedded in sprut.

Method:
- Primary sources only: the posts themselves, specs, official docs, and source code at pinned commits.
- Repos were shallow-cloned and read; none of their code was run.
- Anything I could not check against a primary source is marked **unverified**.
- Third-party measurements are labelled as such, with their method where they give one.

Pinned commits:
- `cloudflare/agents` `5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd` (package `@cloudflare/codemode` 0.5.6)
- `cloudflare/mcp` `563b71037a3cc1f1ed52d944133abb3aca5a0901`
- `openai/codex` `57a808a3920f05f2d4b9eab2d0493e1df136ae6b`

Other repos are listed with their SHA in §3.

---

## Key takeaways for sprut

1. **Two of the three Agents already solve tool discovery natively.**
   - Claude Code defers MCP tools behind tool search by default ([docs](https://code.claude.com/docs/en/agent-sdk/tool-search)).
   - Codex defers every MCP tool behind `tool_search` on supported models ([openai/codex#29486](https://github.com/openai/codex/pull/29486)).
   - Cursor syncs MCP tool descriptions to files ([Cursor blog](https://cursor.com/blog/dynamic-context-discovery)).
   - So the token argument for code mode on the *definition* side is mostly moot for sprut's users. What remains is the *intermediate-result* argument: filtering, joins and loops.
2. **Codex is building client-side Code Mode itself:** an `exec` tool running JS in a V8 isolate that calls `tools.mcp__<server>__<tool>`. It is `UnderDevelopment` and off by default ([`features/src/lib.rs:1301-1306`](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/features/src/lib.rs#L1301-L1306)). Server-side code mode in sprut would nest inside it. Cloudflare explicitly offers `?codemode=false` / opt-in policies for this reason ([cloudflare/mcp README:50-55](https://github.com/cloudflare/mcp/blob/563b71037a3cc1f1ed52d944133abb3aca5a0901/README.md#L50-L55), [portals docs](https://developers.cloudflare.com/cloudflare-one/access-controls/ai-controls/mcp-portals/)).
3. **Biggest product risk: per-tool approval collapses into one `execute` approval.**
   - In Claude Code, `mcp__sprut__linear__save_issue` allow/ask/deny rules ([permissions](https://code.claude.com/docs/en/permissions/)) stop applying to calls made inside a script.
   - The same goes for Codex per-tool `approval_mode` ([config reference](https://learn.chatgpt.com/docs/config-file/config-reference)).
   - Codex's own Code Mode keeps approvals by routing nested calls through its normal MCP handler. A server-side sprut cannot do that.
   - The MCP client best-practices doc says approving a script must not grant blanket approval to its calls ([best practices](https://modelcontextprotocol.io/docs/develop/clients/client-best-practices)).
4. **The evidence is task-shaped, not universal.** Code mode wins big on large-data, long-chain and fan-out tasks: Gatana 10/10 vs 2/10 correct; tmustier −74% cost. It is neutral or worse on short lookups and small catalogs: npow 0.97 vs 0.99 accuracy, Kun Chen slowest condition, IJCA Gemini regression. A **hybrid** (direct tools plus `execute`, with routing guidance) is the best-measured configuration (§6.1).
5. **Every serious implementation converged on one result-mapping rule:** `isError` throws; `structuredContent` wins; otherwise the text is JSON-parsed if possible, else returned as a string ([cloudflare `mcp.ts:60-92`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/mcp.ts#L60-L92), and others in §3). Implementations that ignore `isError` (Docker, mcp-use) are buggy.
6. **outputSchema is a minority feature.** Third-party scans find 27–39% of tools, or 30–46% of servers ([theopenapps](https://theopenapps.com/research/mcp-tool-design-census), [Glimind](https://glimind.com/caniuse)), so most generated return types are `unknown`. Cloudflare, Codex and thehumanworks use `unknown`; pctx and UTCP use `any`.
7. **Engine: a cgo-free static binary rules out V8.**
   - goja + esbuild costs about +10.4 MB on sprut's 12 MB binary. It has `Interrupt()` but **no memory cap**, and Interrupt does not stop native built-ins.
   - modernc.org/quickjs costs +2.6 MB and has timeout, interrupt and memory limit built in, but a smaller user base.
   - fastschema/qjs is unsafe by default (mounts CWD, writes to stdout, no-op timeout).
   - Details in §5.
8. **Go precedent exists.** Docker MCP Gateway (goja, experimental, no limits), gridctl, Limen, ToolMesh and gtoxlili (goja), thehumanworks/codemode-go (QuickJS on wazero, built on the same go-sdk), and Bifrost and ToolHive (Starlark). LeanProxy-MCP spiked goja in a child process and concluded **no-go**, citing approval collapse and invisible chained exfiltration.
9. **The spec has nothing normative on code mode.** The progressive-disclosure interest group calls code mode "orthogonal" to tool search ([discussion #3416](https://github.com/modelcontextprotocol/modelcontextprotocol/discussions/3416)). Within one connection, the 2026-07-28 `tools/list` "MUST NOT vary … as a side effect of other requests" ([tools spec](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)), so "search then expose tools" via `list_changed` is not an option there.
10. **Elicitation needs a design.** In 2026-07-28 it arrives as MRTR `InputRequiredResult` ([MRTR](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr)). sprut today turns `NeedsInput()` into an error ([`internal/gateway/gateway.go:374`](../../internal/gateway/gateway.go)). Inside a script, this needs either the same refusal or an abort-and-replay design like Cloudflare's durable runtime (§2.1).
11. **Agent-side limits constrain `execute`.**
    - Codex's per-tool timeout defaults to 60 s (`tool_timeout_sec`).
    - Claude Code caps MCP output at 25,000 tokens and saves text over 50,000 characters to a file ([Claude Code MCP docs](https://code.claude.com/docs/en/mcp)).
    - Cloudflare caps results at about 6,000 tokens.
12. **Security.** Blocking `fetch` in the sandbox does not stop exfiltration through Upstream tools themselves, for example reading one Upstream and posting to another. The MCP best-practices doc calls this out explicitly ("output truncation alone does not prevent exfiltration").
13. **Cloudflare's own gateway (MCP server portals) is the closest analog to sprut.**
    - It ships code mode as a per-portal policy (`off`, `opt_in` (the default), `default_on`, `enforced`).
    - Two tools: `portal_codemode_search` and `portal_codemode_execute`.
    - It refuses Upstreams that run their own code mode ([portals docs](https://developers.cloudflare.com/cloudflare-one/access-controls/ai-controls/mcp-portals/)).

---

## 1. The primary posts

### 1.1 Cloudflare, "Code Mode: the better way to use MCP" (2025-09-26, Varda & Pai)

Source: https://blog.cloudflare.com/code-mode/

- **Thesis.** LLMs have "seen a lot of code" but only "a small set of contrived examples of tool calls". Code also lets the model chain calls without routing intermediate results through the model.
- **Shape.**
  - The SDK fetches the MCP server's schema and generates a TypeScript API: an `interface XInput` / `XOutput` per tool, and a `declare const codemode: { tool: (input) => Promise<Output> }` with JSDoc from descriptions.
  - In the post's example, every output is `interface …Output { [key: string]: any }`, because the gitmcp server has no outputSchema.
  - "Currently, the entire API is loaded" into context; search/browse was named as future work.
- **One tool**, "which simply executes some TypeScript code".
- **Output** is returned "by invoking `console.log()`"; "all the output logs are passed back to the agent". The current SDK code also returns the function's return value (§2.1).
- **Sandbox.** A V8 isolate created per snippet through the new **Worker Loader** API, with `globalOutbound` able to redirect or block `fetch()`/`connect()`. In Code Mode "the global `fetch()` and `connect()` functions throw errors".
- **Bindings.** MCP servers are reached through RPC bindings, so "the AI cannot possibly write code that leaks any keys".
- **Numbers: none.** The post has no token or accuracy measurements; the claims are qualitative.

### 1.2 Cloudflare, "Code Mode: give agents an entire API in 1,000 tokens" (2026-02-20, Carey)

Source: https://blog.cloudflare.com/code-mode-mcp/

- **Server-side code mode** for the whole Cloudflare API (more than 2,500 endpoints). It has two tools; the post quotes the full surface:
  - `search`: "Search the Cloudflare OpenAPI spec. All $refs are pre-resolved inline." Input `{code: string}` described as "JavaScript async arrow function to search the OpenAPI spec".
  - `execute`: "Execute JavaScript code against the Cloudflare API." Input `{code}`.
- **Discovery** is code too. `search` runs model-written JS over a `spec` object (the full OpenAPI with `$ref`s resolved). "The full OpenAPI spec never enters the model context."
- **Execute** gets `cloudflare.request({method, path, body})`. The isolate has "no file system, no environment variables … external fetches disabled by default".
- **Numbers.**
  - "reduces the number of input tokens used by 99.9%"; "An equivalent MCP server without Code Mode would consume 1.17 million tokens", "measured with tiktoken".
  - The repo README gives the table: native full schemas 2,594 tools / 1,170,523 tokens; native required-params-only 244,047; raw OpenAPI about 2,000,000; code mode 3 tools / about 1,100 ([README:7-12](https://github.com/cloudflare/mcp/blob/563b71037a3cc1f1ed52d944133abb3aca5a0901/README.md#L7-L12)).
  - These are **definition-size counts only**. No task success, accuracy or latency numbers are published.
  - The repo has a manual "blind" agent bench harness (`bench/README.md`) but no published results.
- **Positioning against alternatives** (from the post):
  - Client-side code mode "requires the agent to ship with secure sandbox access".
  - CLIs need a shell.
  - Claude Code's tool search "requires a search function that must be maintained and evaluated, and each matched tool still uses tokens".
- **Forward-looking:** "first-class Code Mode integration for all your MCP servers" in MCP Server Portals. This shipped; see §2.3.

### 1.3 Anthropic, "Code execution with MCP" (2025-11-04, Jones & Kelly)

Source: https://www.anthropic.com/engineering/code-execution-with-mcp

- **Two problems:** tool definitions overload context, and intermediate results cost tokens. Example: a 2-hour transcript copied twice, "an additional 50,000 tokens".
- **Proposed shape: a filesystem of generated TS files**, `servers/<server>/<tool>.ts` plus `index.ts`. Each file is a typed wrapper calling `callMCPTool<Resp>('google_drive__get_document', input)`. Note the `server__tool` naming, the same convention sprut uses.
- **Discovery** is by listing `./servers/` and reading the needed files. The alternative is a `search_tools` tool with a detail-level parameter (name only, name + description, or full schema).
- **Number:** "reduces the token usage from 150,000 tokens to 2,000 tokens … 98.7%". **No methodology given.** It is presented as an illustrative scenario, not a benchmark.
- **Further benefits:**
  - Filtering in code ("The agent sees five rows instead of 10,000").
  - Control flow (polling loops).
  - **Privacy:** "intermediate results stay in the execution environment by default". The harness "can tokenize sensitive data automatically" (`[EMAIL_1]`) and untokenize when the data flows to another tool. This is described as a possibility; **no implementation is referenced** (unverified as a shipping feature).
  - **State and skills:** writing files to `./workspace/`, and saving reusable functions to `./skills/` with a `SKILL.md`. This assumes a filesystem-backed sandbox.
- **Caveat stated:** "Running agent-generated code requires a secure execution environment with appropriate sandboxing, resource limits, and monitoring … operational overhead and security considerations that direct tool calls avoid."

### 1.4 Anthropic, "Introducing advanced tool use" (2025-11-24)

Source: https://www.anthropic.com/engineering/advanced-tool-use

This is the productized follow-up. Its numbers are internal, and methodology is not published beyond benchmark names.

- **Tool Search Tool:**
  - Opus 4 accuracy went from 49% to 74%, and Opus 4.5 from 79.5% to 88.1%, "on MCP evaluations".
  - About 85% fewer definition tokens: roughly 77K to 8.7K in their example.
- **Programmatic Tool Calling (PTC):**
  - Average tokens fell from 43,588 to 27,297 (−37%) "on complex research tasks".
  - Accuracy: internal knowledge retrieval 25.6% to 28.5%; GIA 46.5% to 51.2%.
- **Guidance:** PTC is "less beneficial" for "simple single-tool invocations", "tasks where Claude should see and reason about all intermediate results", and "quick lookups". Tool search is less beneficial below 10 tools. The post recommends documenting return formats in descriptions, since code must parse outputs.

### 1.5 MCP official "Client Best Practices", section "Programmatic Tool Calling / Code Mode"

Source: https://modelcontextprotocol.io/docs/develop/clients/client-best-practices

This is the only MCP-project document that addresses code mode directly. It is non-normative and written for **hosts**, not servers.

- **Generate typed functions** "based on each tool's arguments and `outputSchema`". When outputSchema is absent: "Accept `any` or `string` … The real fix is for server authors to provide `outputSchema`". Alternatively, a host-brokered `extract(value, ExpectedType)` helper backed by a small model.
- **Sandbox table:** Deno / isolated-vm, Monty (Python), pctx, and Wasmtime "Any (via Wasm) / Rust / C / Go".
- **Security considerations, quoted:**
  - "Approving the script does not grant blanket approval for every tool call it makes at runtime; hosts may grant categorical approval … but the broker must still evaluate each call against that grant."
  - "Tool results from one server are untrusted input to another … output truncation alone does not prevent exfiltration."
  - No network access in the sandbox, no credential exposure, timeouts and memory limits, and output filtering.
- **Errors:** "Generated wrappers should convert [`isError`] into a thrown exception … If an uncaught error terminates the script, surface it as the script's result … the model is responsible for reporting any partial side effects already committed."
- **Prompt caching:** "Adding or removing tool definitions mid-conversation invalidates that cache … route every call through a single stable `call_tool({name, args})` meta-tool so the array never changes."

---

## 2. Cloudflare source code

### 2.1 `@cloudflare/codemode` (cloudflare/agents `packages/codemode`, v0.5.6, "Experimental")

There are three entry points with different discovery models.

**(a) `createCodeTool`: everything in the description** ([`src/tool.ts:89-122`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/tool.ts#L89-L122)). This is a single AI-SDK tool with input `{ code: string }` ("JavaScript async arrow function to execute", `tool.ts:22-24`). The description template ([`src/shared.ts:10-19`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/shared.ts#L10-L19)):

```
Execute code to achieve a goal.

Available:
{{types}}

Write an async arrow function in JavaScript that returns the result.
Do NOT use TypeScript syntax — no type annotations, interfaces, or generics.
Do NOT define named functions then call them — just write the arrow function body directly.

Example: async () => { const r = await codemode.searchWeb({ query: "test" }); return r; }
```

Note that it asks for **JavaScript, not TypeScript**, even though the types are shown as TS. No transpiler is used.

**(b) `codeMcpServer`: wraps an MCP server into one `code` tool** ([`src/mcp.ts:96-234`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/mcp.ts#L96-L234)).
- Same template, plus an `{{example}}` built from the first tool's input properties.
- It builds descriptors from `inputSchema` **only** (`mcp.ts:141-147`), so outputSchema is dropped here and every output is `unknown`.
- Errors return `isError: true` with `Error: <msg>`.
- Results go through `responseText`, capped at `MAX_RESPONSE_CHARS = 24_000` ("~6,000 tokens", `mcp.ts:24-25`) with structural JSON truncation ([`src/truncate.ts`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/truncate.ts); CHANGELOG 0.5.2 explains why slicing JSON was replaced).

**(c) Connector runtime (`createCodemodeRuntime`, `McpConnector`): discovery inside the sandbox.**
- The model calls `codemode.search("short intent phrase")` and `codemode.describe("connector.method")` (which returns TS declarations) inside its code, then calls `<connector>.<method>(args)`.
- The tool description ([`src/proxy-tool.ts:553-602`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/proxy-tool.ts#L553-L602)) includes:
  - "Never guess method names. If you have not used a connector in this conversation, run a discovery pass first"
  - "Some methods require approval. The run pauses until the user approves, then resumes automatically. Write code as if the call returns normally."
  - "All code outside connector calls and `codemode.step` must be deterministic so resume can replay it."
- Search is weighted token matching, not BM25: fields path 12, connector 8, method 10, description 5; at most 50 results ([`src/connectors/search.ts:11-18`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/connectors/search.ts#L11-L18)).
- `McpConnector` **does** forward `outputSchema` (`src/connectors/mcp.ts:130`; CHANGELOG 0.5.4).

**JSON Schema → TS** ([`src/json-schema-types.ts`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/json-schema-types.ts)):
- Handles `$ref` (with a cycle guard), anyOf/oneOf as unions, allOf as intersection, enum/const as literal unions, prefixItems as tuples, `nullable`, and `format` as `@format` JSDoc.
- `maxDepth: 20` (`:283-285`); deeper levels become `unknown`.
- **No outputSchema means `type XOutput = unknown`** (`:345-349`).
- A conversion failure makes both input and output `unknown` (`:379-385`).
- The JSDoc carries the tool description plus `@param input.<field> - <desc>`.
- Names go through `sanitizeToolName` (`src/utils.ts:74-90`): `-`, `.` and spaces become `_`, other non-identifier characters are stripped, and a leading digit gets `_`.

**Result unwrapping** ([`mcp.ts:60-92`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/mcp.ts#L60-L92)), in order:
1. `isError` throws `Error(text)`.
2. `structuredContent`.
3. All-text content is joined and `JSON.parse` is tried, falling back to the string.
4. Mixed content (images, resources) is returned as the raw result.

**Executor** ([`src/executor.ts`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/executor.ts)):
- Timeout 60,000 ms by default (`:261`), implemented as `Promise.race` against `setTimeout` inside the isolate (`:500-511`).
- `globalOutbound` defaults to `null`, which blocks network (`:322`).
- `console.log/warn/error` are captured into `__logs` (`:489-492`).
- Return shape: `{result, logs}` or `{error, logs}`. `runCode` throws `Code execution failed: … Console output: …` (`src/run-code.ts:22-27`).
- Code normalization uses acorn: it strips markdown fences, accepts an arrow function, `export default`, or a named function that it wraps and calls (`src/normalize.ts`).

**Approvals, two different behaviours:**
- `createCodeTool` / `resolveProvider` **silently drop** AI-SDK tools marked `needsApproval` ([`src/resolve.ts:16-31`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/resolve.ts#L16-L31)), so approval-gated tools are simply not callable from code.
- The connector runtime implements **abort-and-replay** ([`src/runtime.ts:1-49`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/packages/codemode/src/runtime.ts#L1-L49); [`docs/codemode/approvals.md`](https://github.com/cloudflare/agents/blob/5c33d89791d6a00c6ea64cf7790fa753e3e5cbfd/docs/codemode/approvals.md)):
  - Every connector call and every `codemode.step(name, fn)` is recorded in a durable log (SQLite in a Durable Object facet).
  - A `requiresApproval` call records a pending entry and aborts the run. The tool returns `{status:"paused", executionId, pending}`.
  - After approval the same code re-runs; logged calls are served from the log and the approved call executes for real.
  - Divergence on replay is an error.
  - This is the only implementation found that preserves **per-call** human approval inside a script.

### 2.2 Cloudflare API MCP server (`cloudflare/mcp`)

- **Tool shapes as deployed** are richer than the blog.
  - `execute` takes `{code, account_id?, truncate?}`. Its description is [`src/tools/execute.ts:395-414`](https://github.com/cloudflare/mcp/blob/563b71037a3cc1f1ed52d944133abb3aca5a0901/src/tools/execute.ts#L395-L414): "…First use the 'search' tool to find the right endpoints, then write code using the cloudflare.request() function … A failed request throws an Error naming the method, path, HTTP status and Cloudflare's errors…"
  - Annotations: `execute` `{readOnlyHint:false, openWorldHint:true, destructiveHint:true}` (`execute.ts:456-460`); `search` `readOnlyHint:false` (`src/tools/search.ts:89`).
  - The search description embeds up to 30 product names, `SPEC_TYPES`, and three worked examples (`search.ts:25-60`).
  - The descriptions are deliberately identical for every token, because "MCP clients cache tool metadata and may serve one user's tool list to another" (`execute.ts:388-394`).
- **Network is proxied, not blocked** (this corrects the "fetch blocked" summary).
  - `GlobalOutbound` (`execute.ts:60-90`) allows only the API host and **injects the bearer token outside the user-code isolate**.
  - The sandbox wraps `fetch` to record whether every request was a read (`replaySafe`), to decide whether re-running after an OAuth scope step-up is safe.
- **Isolate per call.** "the user code is baked into the module source (dynamic-worker isolates disallow eval)" (`execute.ts:94-99`).
- **Truncation.** About 6,000 tokens by default (`src/truncate.ts:20-21`). The `truncate:false` argument is described as "Set to false only when the result goes straight to a file or pipe" (`:38-39`).
- **Composition with client-side code mode:** `?codemode=false` registers about 3,000 direct tools generated by forge "If your MCP client already uses code mode, or you're composing this server with another server that uses code mode" ([README:50-77](https://github.com/cloudflare/mcp/blob/563b71037a3cc1f1ed52d944133abb3aca5a0901/README.md#L50-L77)).

### 2.3 Cloudflare MCP server portals (a gateway, like sprut)

Sources: [docs](https://developers.cloudflare.com/cloudflare-one/access-controls/ai-controls/mcp-portals/), [changelog 2026-03-26](https://developers.cloudflare.com/changelog/post/2026-03-26-mcp-portal-code-mode/), [GA 2026-09-24](https://developers.cloudflare.com/changelog/post/2026-09-24-mcp-portals-ga/)

- **History.** The March changelog said code mode was "turned on by default" and exposed a single `code` tool. Current docs describe two tools, `portal_codemode_search` and `portal_codemode_execute`, which call "typed `codemode.*` methods" in a Dynamic Worker.
- **Policy per portal:**

  | Policy | Effect |
  |---|---|
  | `off` | no code mode |
  | `opt_in` | **default**; client enables with `?codemode=search_and_execute` |
  | `default_on` | on; client disables with `?codemode=off` |
  | `enforced` | always on |

  The docs give the rationale: "Use Opt-in or On by default if some clients run their own Code Mode implementation. These policies let clients avoid nested code execution."
- **"MCP portals do not support upstream MCP servers that have their own Code Mode turned on."**
- The portal speaks 2026-07-28 and earlier Streamable HTTP. Per-tool calls are logged by Access, so audit granularity is kept on the gateway side even though the Agent sees one tool.

---

## 3. Other implementations

Delegated survey; repos shallow-cloned and grepped, none run. HEAD SHAs as of 2026-10/11.

### 3.1 Go

| Project | Engine | Tools exposed | Discovery | Results / errors | Limits | Notes |
|---|---|---|---|---|---|---|
| [docker/mcp-gateway](https://github.com/docker/mcp-gateway) `7e0f342` (go-sdk) | goja in-process (`pkg/codemode/exec.go:14`) | `code-mode {servers[], name}` *creates* a tool `code-mode-<name> {script}`, called via `mcp-exec` (`dynamic_mcps.go:154-184`, `pkg/gateway/codemode.go:113-135`) | full pseudo-TS listing in the generated description; `fn(args): string` (outputSchema ignored); prompt says calls are **synchronous** (`codemode.go:12-26`) | only `Content[0]` text; **`isError` ignored** (`exec.go:93-100`); a script error comes back as a *successful* "Error running script" result | **none**: no Interrupt, ctx or memory cap | Docs: "early development and is not yet reliable" ([docs](https://docs.docker.com/ai/mcp-catalog-and-toolkit/dynamic-mcp/)). `console` writes to `os.Stdout`, which would corrupt stdio MCP (inferred, untested) |
| [gridctl/gridctl](https://github.com/gridctl/gridctl) `a35ad8e` | goja + goja_nodejs loop; **esbuild** lowers to ES2015 (`pkg/mcp/codemode_transpile.go:12-32`) | `search {query}`, `execute {code}`, **replacing** the tool list | substring over name, description and params; returns raw inputSchema JSON | `mcp.callTool(server, tool, args)` is **synchronous**; `isError` throws; structured → JSON → string; MRTR input-required throws | 64 KB code, 30 s, then Interrupt | per-Agent ACL on `server__tool`; includes a `fetch()` (HTTPS only, private CIDRs blocked) |
| [Belphemur/limen](https://github.com/Belphemur/limen) `cfcfc72` (mcp-go) | goja + loop, truly async via `RunOnLoop` | `codemode_search {code}`, `codemode_execute {code}` | in-sandbox `codemode.tools(filter)` / `schemas(names)` | raw `CallToolResult`, unwrapped with `codemode.json()` | 50 calls (uncatchable Interrupt), 8 concurrent, 30 s; documented 64 MB heap cap **not implemented** (`config.go:236,407`) | multi-tenant |
| [thehumanworks/codemode-go](https://github.com/thehumanworks/codemode-go) `1001d87` (**go-sdk v1.8.0**, same as sprut) | QuickJS-ng WASM on wazero (fastschema/qjs v0.0.6, own wasm build without libc) | `code {code, timeout?}`; modes on/only/off | in-script `await search(q)` (BM25 k1 1.2, b 0.75) and `describe(path)` returning `type Args`/`type Result`; **absent outputSchema means `unknown`** (`catalog/tsdecl.go:54`) | `isError` rejects; structured → JSON text → string; output `<result>…</result><console_output>…` plus structuredContent `{ok,result,calls,elapsed_ms,truncated}` | 60 s (max 10 min), 200 calls, 8 concurrent, 64 MiB memory, 24 KiB result text | wraps the server's **own** tools; DESIGN.md lists fastschema/qjs pitfalls; parity test against a wire client |
| [gtoxlili/codemode-go](https://github.com/gtoxlili/codemode-go) `ffda6c1` | goja + goja_nodejs | `run_code`, additive | none (no TS) | catchable `ToolCallError`; failure taxonomy worded for the model | 10 min wall, **2 min JS compute**, 256 MB measured heap growth, 200 calls | README: "this is not a security sandbox"; limits sampled every 50 ms |
| [mmornati/leanproxy-mcp](https://mmornati.github.io/leanproxy-mcp/design/code-mode/) `2e4aa8e` | goja in a **child process** of the same binary (rlimits, empty env, `Pdeathsig`) | `execute_code {code}`, `destructiveHint:true` | gateway's own `search_tools`; no stubs ("would bring back the schema tax") | same mapping as thehumanworks | 32 calls, 4 concurrent, 4 KiB console | **Verdict "No-go for v1.0".** Measured goja +6.1 MiB / +31% binary; 0.05 ms in-process vs 18 ms p50 child process; risks: approval collapse, invisible chained exfiltration |
| [maximhq/bifrost](https://github.com/maximhq/bifrost) `6e64027` | **Starlark** | `listToolFiles`, `readToolFile`, `getToolDocs`, `executeToolCode` (virtual FS of `.pyi` stubs) | file listing | stubs always `-> dict` (outputSchema unused) | 30 s per tool | vendor benchmark, §6.1 |
| [stacklok/toolhive](https://github.com/stacklok/toolhive) vMCP `3738fc9` | **Starlark** | `execute_tool_script {script, data?}` **alongside** backend tools | names plus 80-char descriptions in the description | `isError` → error; structured; else first text JSON-parsed | 100k steps, 10 concurrent, 30 s per call | inner calls go back through Cedar authz; client `_meta` not forwarded |
| [DunkelCloud/ToolMesh](https://github.com/DunkelCloud/ToolMesh) `895a7ab` | goja | `discover_tools {pattern?,query?,detail?,limit?}`, `execute_code` | regex/BM25, detail auto-scaled, 50 KB cap | — | 120 s; static scan before running | before/after policy "Gate" per call |

Also found (README-level): ethpandaops/ogen-mcp (goja, Cloudflare-style search/execute from OpenAPI) and containers/kubernetes-mcp-server PR #711 (goja `evaluate_script`).

**Absent** (searched, not grepped, so unverified): MetaMCP, MCPJungle, IBM ContextForge, agentgateway and Microsoft mcp-gateway show no code-mode feature.

### 3.2 Non-Go (selected)

**[UTCP code-mode](https://github.com/universal-tool-calling-protocol/code-mode)** `e5fdc31` (TS/Python)
- Runtime: isolated-vm, 128 MB, 30 s; calls look synchronous via `applySyncPromise`.
- Tools: `search_tools {task_description, limit}`, `tools_info`, `call_tool_chain {code, timeout, max_output_size}`.
- Generates `interface tInput/tOutput`; a missing schema becomes `any`.
- Says "TypeScript" but has no transpiler in `src/` (inferred).

**[pctx](https://github.com/portofcontext/pctx)** `499379d` (Rust, deno_core)
- **Type-checks with the real TS compiler** before running.
- Three disclosure modes:
  - `catalog`: `list_functions`, `get_function_details`, `execute_typescript`
  - `filesystem`: `execute_bash` over a read-only virtual FS of `.d.ts` files, plus `execute_typescript`
  - `sidecar`: the original tools stay listed
- Code must define `async function run()`. No output schema means `any`.
- `DEFAULT_TIMEOUT` 120 s (the docs' "10 s" is stale). A process-wide `V8_MUTEX` serializes runs.
- No upstream OAuth.

**[mcp-use](https://github.com/mcp-use/mcp-use)** `ac0b6a5`
- Tools: `execute_code`, `search_tools {query, detail_level}`.
- Runtime: **`node:vm`**, not a security boundary. `memoryLimitMb` is unused, and `isError` is ignored.

**[jx-codes/lootbox](https://github.com/jx-codes/lootbox)** `587a5a1`
- Deno subprocess with `--allow-net`, meaning unrestricted network. 10 s timeout. Not an MCP server.

**[domdomegg/tool-sandbox-mcp](https://github.com/domdomegg/tool-sandbox-mcp)**
- Single `execute_code {code, store_id?}`, `destructiveHint:true`, quickjs-emscripten.
- Proxies the upstream's OAuth.
- Returns up to 5 image/audio blobs as MCP content.

**[acoyfellow/mcp-code-mode](https://github.com/acoyfellow/mcp-code-mode)** `f27af39`
- `search` + `execute`.
- `expose` is a fail-closed allowlist; **`keepNative`** keeps destructive tools as direct calls.

**[postrv/forgemax](https://github.com/postrv/forgemax)** (Rust)
- `search {code}` over a `manifest` global, plus `execute`.
- Defaults: 5 s, 64 MB, 50 calls, 1 MB output.
- oxc AST pre-scan; child-process worker.

### 3.3 Academic antecedent: CodeAct

Source: [Wang et al., ICML 2024, arXiv 2402.01030](https://arxiv.org/abs/2402.01030)

- On M³ToolEval (82 multi-tool tasks), gpt-4-1106 succeeded 74.4% with code vs 52.4% with JSON, in 5.5 vs 7.6 turns.
- Code was best in 12 of 17 models.
- On API-Bank (single atomic calls), **JSON beat code** for gpt-4-1106: 82.7 vs 76.7.
- Small author-built benchmark, no confidence intervals.

### 3.4 Cross-cutting observations

- **Discovery styles:**
  - everything in the description: Cloudflare `createCodeTool`, Docker, ToolHive
  - search tool returning schemas or TS: gridctl, UTCP, mcp-use, ToolMesh, pctx catalog, Cloudflare API server
  - discovery inside the script: Cloudflare connectors, thehumanworks, Limen, Codex `ALL_TOOLS`
  - virtual filesystem: Anthropic post, pctx filesystem mode, Bifrost, Cursor (client-side)
- **Replace vs additive:** gridctl, Limen and Bifrost replace the tool list; ToolHive, thehumanworks `on`, pctx `sidecar`, gtoxlili and mcp-code-mode `keepNative` are additive.
- **Policy:** serious gateways re-enter their normal per-call path (authz, logging, timeouts) for each inner call.

---

## 4. MCP spec relevance (2026-07-28)

### 4.1 outputSchema and structuredContent

From the [tools spec](https://modelcontextprotocol.io/specification/2026-07-28/server/tools):
- `outputSchema` is optional, JSON Schema 2020-12 by default. If present, "Servers MUST provide structured results that conform … Clients SHOULD validate".
- `structuredContent` can be any JSON value. "a tool that returns structured content SHOULD also return the serialized JSON in a TextContent block". This is why "parse text as JSON" works as a fallback.

**How common** (third-party scans; their methods differ, none are primary):

| Source | Scope | Tools with outputSchema | Servers with ≥1 |
|---|---|---|---|
| [theopenapps census](https://theopenapps.com/research/mcp-tool-design-census), 2026-10-13 | 425 open remote servers, 7,777 tools | 39% | 46% |
| [Glimind](https://glimind.com/caniuse), 2026-11-07 | 15,859 servers | — | 32.1% |
| [INITE](https://inite.ai/en/atlas/mcp/report) | — | — | 30% |
| [The Ops Log](https://dev.to/theopslog/mcp-schema-drift-isnt-a-rate-its-a-small-set-of-servers-that-never-stop-moving-243c), corrected after de-duplicating one gateway | — | about 27% | — |

[One fault-injection test](https://dev.to/tim860/i-tested-31-mcp-servers-for-contract-compliance-only-3-passed-25gp) of 31 servers found that 42% of declared schemas are "vacuous", for example FastMCP's `{"result": string}` wrapper.

### 4.2 Annotations

- `readOnlyHint`, `destructiveHint`, `idempotentHint` and `openWorldHint` exist, but "clients MUST consider tool annotations to be untrusted unless they come from trusted servers" ([tools spec](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)).
- Prevalence: Glimind 62% of servers have read-only/destructive hints; theopenapps 91% of tools set `readOnlyHint`.
- Agents use them:
  - Codex has `destructive_enabled` and `open_world_enabled` per app ([config reference](https://learn.chatgpt.com/docs/config-file/config-reference)).
  - Claude Code honors `_meta["anthropic/requiresUserInteraction"]` to force a prompt even under allow rules ([Agent SDK permissions](https://code.claude.com/docs/en/agent-sdk/permissions)).

### 4.3 Constraints on dynamic tool surfaces

- `tools/list` "MUST NOT vary per-connection or as a side effect of other requests on the connection" but "MAY vary by the authorization presented on the request" ([tools spec](https://modelcontextprotocol.io/specification/2026-07-28/server/tools)).
- A gateway therefore can't expose tool X *because* the Agent just searched for it (Docker's `mcp-add` pattern) without violating this. In-band discovery must go through tool results.
- Tool names: 1–128 characters from `[A-Za-z0-9_.-]`. "proxies that aggregate tools … SHOULD implement a disambiguation strategy such as prefixing tool names with a server identifier". sprut already does this with `__`.

### 4.4 Human in the loop

The tools spec says:
- "there SHOULD always be a human in the loop with the ability to deny tool invocations"
- Clients SHOULD "Show tool inputs to the user before calling the server, to avoid malicious or accidental data exfiltration"
- Clients SHOULD "Implement timeouts for tool calls"

With server-side code mode, the user sees only the script, never the inner calls' inputs.

### 4.5 Elicitation, sampling and roots: MRTR

- In 2026-07-28, server→client requests are **only** carried via `InputRequiredResult {inputRequests, requestState}` on `tools/call`, `prompts/get` and `resources/read`. The client retries with `inputResponses` and echoes `requestState` ([MRTR](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/mrtr), [SEP-2322](https://modelcontextprotocol.io/seps/2322-MRTR)).
- `requestState` is opaque. If it influences authorization or business logic, servers MUST integrity-protect it (HMAC/AEAD) and SHOULD bind it to principal, TTL and request.
- Servers MUST NOT send input request types the client didn't declare.
- **Implication.** An `execute` call whose inner Upstream call returns `input_required` could itself return `input_required` to the Agent, putting the script's replay log in `requestState`. That is Cloudflare's abort-and-replay (§2.1) mapped onto MRTR. It is not implemented anywhere I found.
- gridctl throws instead. sprut's direct path currently returns an error result ([`gateway.go:374`](../../internal/gateway/gateway.go)).

### 4.6 Progress and cancellation

- Progress: optional `notifications/progress` keyed by `progressToken` ([progress](https://modelcontextprotocol.io/specification/2026-07-28/basic/utilities/progress)). An `execute` could aggregate inner progress, but Claude Code's per-server `timeout` is "a hard wall-clock limit per tool call, and progress notifications from the server don't extend it" ([Claude Code MCP docs](https://code.claude.com/docs/en/mcp)).
- Cancellation: on stdio the client MUST send `notifications/cancelled`; on Streamable HTTP a disconnect is the cancellation ([cancellation](https://modelcontextprotocol.io/specification/2026-07-28/basic/utilities/cancellation)). A sandbox must map Agent cancellation onto script interrupt plus inner-call ctx cancellation.

### 4.7 Proposals and working groups (none normative)

- [SEP-1821](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1821) "Dynamic Tool Discovery": `tools/list` `query` param plus `tools.filtering` capability. Draft, no sponsor.
- [SEP-1881](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1881) scope-filtered discovery.
- [SEP-1888](https://github.com/modelcontextprotocol/modelcontextprotocol/issues/1888) "Progressive Disclosure for Typed Library Discovery": a `<library>.searchTools` meta-tool for code generation. Closed.
- [SEP-2636](https://github.com/modelcontextprotocol/modelcontextprotocol/pull/2636) "Tool Manifests" (`tools/manifest` + `tools/describe` with digests). Closed. It "makes no claim about model context tokens".
- [Discussion #1923](https://github.com/modelcontextprotocol/modelcontextprotocol/discussions/1923) `tools/list_summary`.
- Progressive Disclosure interest group:
  - [#3375, 2026-09-21](https://github.com/modelcontextprotocol/modelcontextprotocol/discussions/3375): server-side search+execute "causes a loss of native tool calling benefits"; "nesting code mode when both servers and clients use code execution patterns can cause performance conflicts"; client tool search costs a model turn and risks cache-busting.
  - [#3416, 2026-10-05](https://github.com/modelcontextprotocol/modelcontextprotocol/discussions/3416): ratified the problem statement and agreed "code mode is orthogonal to the tool-search discussion". A `groups/list` / `groups/get` straw man is under review.

---

## 5. Embedding a JS sandbox in Go

Delegated research; binary sizes **measured** by building sprut's tree with its release flags.

sprut constraints:
- `.goreleaser.yaml:8-14` sets `CGO_ENABLED=0`, linux/darwin × amd64/arm64, `-trimpath`, `-s -w`.
- The base binary is about 12.08 MB (darwin/arm64).
- `go install` users build with their own toolchain.

| Engine | cgo | ES level | Async interop with Go | Timeout | Memory cap | Δ sprut binary | Maintenance |
|---|---|---|---|---|---|---|---|
| [goja](https://github.com/dop251/goja) | no | ES5.1+ and most ES6+: async/await (since 2022-12), generators, Promise, for-await (2026-09). **No async generators, no ESM** | `NewPromise()`; resolve only on the VM goroutine; Runtime is not goroutine-safe | `Interrupt()`, goroutine-safe; **doesn't stop native built-ins or regexp2** | **none**: "Not in scope" ([issue #6](https://github.com/dop251/goja/issues/6)); `'x'.repeat(…)` OOM-kills ([#645](https://github.com/dop251/goja/issues/645)) | +6.69 MB (+6.75 with goja_nodejs) | very active; no tags; 13 Fuzzilli host crashes reported 2026-11 (#848) |
| [grafana/sobek](https://github.com/grafana/sobek) | no | goja + experimental ESM / top-level await | as goja | as goja | none | +6.86 MB | active (k6) |
| [modernc.org/quickjs](https://pkg.go.dev/modernc.org/quickjs) v0.27.0 | **no** (ccgo-transpiled C) | ES2023, incl. modules, async generators, BigInt; no Intl | `NewPromiseCapability` + `ExecutePendingJobs` driven by your loop | `SetEvalTimeout`; `Interrupt()` from any goroutine | `SetMemoryLimit`, `SetMaxStackSize` (catchable) | **+2.61 MB** | v0.27.0, 2026-10-24; smaller user base; slower than goja on V8-v7 bench (323 vs 442, author's numbers) |
| [fastschema/qjs](https://github.com/fastschema/qjs) v0.0.6 (QuickJS-ng on wazero) | no | ES2023 | `SetAsyncFunc` | `MaxExecutionTime` is a **no-op** (issue #27); ctx close via wazero works | QuickJS limit + wazero pages | +4.08 MB | **stale** (last commit 2025-10). **Unsafe defaults:** mounts CWD read/write, `std`/`os` on globalThis, stdout to `os.Stdout`, which would corrupt sprut's stdio MCP |
| aperturerobotics/go-quickjs-wasi-reactor | no | ES2023 | raw C API; you write the glue | wazero ctx | `WithMemoryLimitPages` (hard) | ≈ +3.7 MB (estimated, not built) | active, 15★ |
| otto | no | ES5 only; no Promise | — | interrupt channel | none | +1.57 MB | maintenance only |
| [v8go](https://github.com/tommie/v8go) | **yes** (C++, Clang 21) | current V8 | yes | `TerminateExecution` | `WithResourceConstraints` | not buildable cgo-free; prebuilt libv8 about 115–125 MB per platform | breaks the static release |
| starlark-go | no | Starlark (not JS) | synchronous | `Thread.Cancel`, `SetMaxExecutionSteps` | none | +0.73 MB | active; used by Bifrost and ToolHive |
| [esbuild `pkg/api`](https://pkg.go.dev/github.com/evanw/esbuild/pkg/api) v0.28.2 (TS → JS) | no | strips TS, **no type checking**; lowers async and async generators to generators; cannot lower ES6 to ES5 | — | — | — | +3.73 MB; **goja + esbuild +10.41 MB** | active |

Notes:
- **goja:**
  - `goja_nodejs/eventloop.Run()` exits when only timers are pending, not in-flight `RunOnLoop` work, so use `Start` with your own completion signal or a small custom loop.
  - Go panics escape `RunProgram`: `recover` and discard the VM.
  - Set `SetMaxCallStackSize`, which defaults to MaxInt32.
  - Set the global `regexp2.DefaultMatchTimeout`, which defaults to "forever".
  - Go stack overflow and OOM are fatal to the whole process, which would take down every Upstream child of that `sprut serve`.
- **Child-process isolation** (LeanProxy pattern): re-exec `sprut` as a worker with rlimits. It costs about 18 ms per run and gives Linux-only hard limits (`RLIMIT_AS`); macOS has no comparable enforced address-space limit (LeanProxy notes "hard limits work on Linux only").
- **TypeScript:**
  - Cloudflare and Codex both ask for **plain JS** and avoid a transpiler.
  - With esbuild, use `Loader: TS` and a target that keeps generators (ES2017), and wrap code in an async IIFE because goja has no top-level await.
  - typescript-go has no public Go API.
- **Not measured:** per-runtime startup time and esbuild transform latency (unverified).

---

## 6. Critiques, limitations, counter-evidence

### 6.1 Measurements (in roughly decreasing rigour)

| Study | Setup | Result | Caveats |
|---|---|---|---|
| [tmustier/code-mode-mcp benchmark](https://github.com/tmustier/code-mode-mcp/blob/main/docs/benchmarks/2026-07-direct-tools-and-code-mode.md) | 10-tool deterministic server, 8 task shapes × 4 conditions × 5 runs = 160 scored runs, exact JSON scorer | **guided hybrid** (direct + `exec` + routing guidance): −40.6% wall time, −73.7% cost vs direct-only; all correct. Direct better for one-off lookups and 2-step routes; code mode better for an 8-step chain, a 19-call fan-out and large payloads. At 2,000 tools direct "stopped for length before it called a tool" | one model, synthetic server, cache effects |
| [Gatana](https://www.gatana.ai/blog/code-mode-vs-direct-tool-calls-benchmark), 2026-10-05 | DeepSeek-V4.1-Flash, 3 tasks × 5 runs × 2 modes, ground truth computed outside the model | large-data tasks: direct 2/10 correct vs code 10/10; 4–44× fewer prompt tokens; 17–51× faster. Small task: both correct, direct slightly faster | vendor; one model |
| [npow](http://npow.github.io/posts/mcp-beats-code-generation/) | 10-tool API, 27 tasks × 3 models (Haiku, Sonnet, Opus) × 4 approaches × 3 trials = 972 runs; 2-judge LLM ensemble | MCP direct 0.995 vs **code mode 0.969**; search+execute 0.99 but slowest (Opus 30.1 s vs 19.7 s). Code-mode failures are "confident wrong answers" (worst task 0.33 vs 0.83) | p = 0.107; LLM judges; tasks co-designed with the tools |
| [IJCA paper](https://www.ijcaonline.org/archives/volume187/number136/mbat-ijca-2026-e10508249e55.pdf) | 6 scenarios × 8 models | code mode −37.6% tokens, −40.7% cost, pass 59.4% → 63.6%; **+48.3% retries**; Gemini models regress from about 2/3 to about 1/3 pass; worst-case slice: code mode uses *more* tokens | small scenario set |
| [Bifrost vendor benchmark](https://github.com/maximhq/bifrost-benchmarking/blob/main/mcp-code-mode-benchmark/benchmark_report.md) | 96 / 251 / 508 tools, about 65 queries each | input tokens −58% / −85% / −93%; pass 100% both modes (one +1) | vendor; pass criteria not verified |
| [Anthropic advanced tool use](https://www.anthropic.com/engineering/advanced-tool-use) | internal | PTC −37% tokens, +2.9 / +4.7 pt accuracy | methodology unpublished |
| [arXiv 2607.10569](https://arxiv.org/abs/2607.10569) | Claude Code and Codex, `execute_code`-only vs tool-rich | cheaper or tied in 3 of 4 cells; pass rates tied | coding tasks, not MCP integrations |
| [FastMCP issue #4902](https://github.com/PrefectHQ/fastmcp/issues/4902) | 12 tasks | −68% input tokens, **+32% latency**; with a ReAct agent 7/12 code-mode tasks scored 0 | author: "I don't think those numbers generalize" |
| [Kun Chen](https://kunchenguid.medium.com/i-benchmarked-github-cli-vs-mcp-vs-tool-search-vs-code-mode-turns-out-the-best-solution-is-none-93528d5039e4) | GitHub tasks, 5 conditions | code mode cheapest MCP option but **slowest** (43.4 s); unique failure: scripts throw on a nonexistent repo (4/5) | blog |
| LeanProxy harness (§3.1) | fixed programs, no model | 5×`list_issues` → top-3: 289,982 → 230 tokens; one small call: **+201.9%**; truncation plus code gave a **silently wrong** answer | no model in the loop |

Synthesis:
- Code mode helps when data is large or chains are long.
- It hurts or is neutral for short tasks, small catalogs and weaker models (retry loops).
- Latency rises when discovery adds turns.
- Results are more variable.
- No study measured code mode **behind a gateway in front of an Agent that already has tool search**, which is sprut's exact situation.

### 6.2 Security

- **Exfiltration through Upstreams.** A sandbox with no network still has every Upstream tool. Code can read a private repo through one Upstream and write it to a public issue or a Slack channel through another, invisibly to the user. Sources: the MCP best practices ("Cross-server data flow … output truncation alone does not prevent exfiltration") and LeanProxy's no-go rationale.
- **Prompt injection.** Results from one Upstream steer code that acts on another, and the user never sees the intermediate data. Anthropic presents keeping intermediate data out of the context as a *privacy feature*, but the same property removes the human checkpoint.
- **Attack-surface study.** [arXiv 2602.15945](https://arxiv.org/abs/2602.15945) (MAESTRO) identifies "sixteen attack classes across five execution phases", including "exception-mediated code injection and unsafe capability synthesis", and recommends containerized sandboxing plus semantic gating.
- **Sandbox escape and DoS in Go engines.** goja has no heap cap, Interrupt can't stop native code or regexp2, and there are open fuzz crashes. In-process means one bad script can kill the `sprut serve` process and all its stdio Upstreams. `node:vm` style "sandboxes" (mcp-use) are not security boundaries.
- **Credentials.** With in-process bindings the token never enters JS. All implementations hold credentials host-side, and sprut's Login/Credentials model already does.

### 6.3 Interaction with Agent-side approvals (critical for Claude Code and Codex users)

- **Claude Code.**
  - Permission rules are per tool name: `mcp__<server>__<tool>`, `mcp__<server>__*`, globs only after a literal server prefix; deny then ask then allow ([permissions](https://code.claude.com/docs/en/permissions/)).
  - With sprut today, users can allow `mcp__sprut__linear__list_*` and ask on `mcp__sprut__linear__save_*`.
  - Behind `execute`, there is one rule, `mcp__sprut__execute`. Allowing it auto-approves every write in every script; asking on it prompts for every read.
  - PreToolUse hooks see only the script text.
- **Codex.** Per-tool `mcp_servers.<name>.tools.<tool>.approval_mode` (`auto|prompt|writes|approve`), `default_tools_approval_mode`, and `destructive_enabled` / `open_world_enabled` for apps ([config reference](https://learn.chatgpt.com/docs/config-file/config-reference)). All of it collapses to one `execute` tool.
- **Codex's own Code Mode keeps approvals.**
  - Nested calls are re-submitted to the normal `ToolCallRuntime` with `ToolCallSource::CodeMode` ([`core/src/tools/code_mode/mod.rs:338-411`](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/core/src/tools/code_mode/mod.rs#L338-L411)).
  - The MCP handler ([`handlers/mcp.rs:284`](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/core/src/tools/handlers/mcp.rs#L284)) calls `handle_mcp_tool_call`, which calls `maybe_request_mcp_tool_approval` ([`mcp_tool_call.rs:283`, `:1737`](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/core/src/mcp_tool_call.rs#L1737)).
  - So client-side code mode is strictly better than server-side on this axis.
- **Known server-side mitigations:**
  1. Exclude non-read-only tools from code mode and keep them as direct tools (mcp-code-mode `keepNative`; decide via `readOnlyHint`/`destructiveHint`, which are untrusted).
  2. Pause and replay with approval (Cloudflare connector runtime). In MCP terms this means returning MRTR `elicitation/create` from `execute` to ask the user per write. It depends on the Agent supporting elicitation; Codex has `approval_policy.granular.mcp_elicitations`.
  3. Annotate `execute` with `destructiveHint:true` (Cloudflare, LeanProxy, tool-sandbox-mcp) so Agents prompt on it.
  4. A Config-level allowlist of which Upstreams or tools are reachable from code.

### 6.4 Elicitation, sampling, progress, cancellation, limits

- Elicitation and sampling from an Upstream mid-script: see §4.5. Implementations either throw (gridctl) or don't handle it. Cloudflare's SDK auto-fulfills `input_required` on its *client* side (CHANGELOG 0.5.1), which a gateway can't do without the user.
- **Timeouts:**
  - Codex `tool_timeout_sec` defaults to 60 s per tool.
  - Claude Code has a per-server `timeout` / `MCP_TOOL_TIMEOUT`, and progress doesn't extend it.
  - A script fanning out over slow Upstreams can exceed the Agent's timeout. sprut's sandbox timeout must sit below it, and the Agent's cancellation must interrupt the script.
- **Output:** Claude Code warns above 10,000 tokens, caps at 25,000 (`MAX_MCP_OUTPUT_TOKENS`), and saves text over 50,000 characters to a file ([Claude Code MCP docs](https://code.claude.com/docs/en/mcp)). Codex `exec` defaults to `max_output_tokens` 10,000.
- **Rich content:** images and audio from Upstreams get lost in `console.log` designs. Codex has `image()`/`audio()` helpers; tool-sandbox-mcp passes through up to 5 blobs.

### 6.5 Nesting

- If the Agent runs client-side code mode (Codex `code_mode`, under development) and sprut runs server-side code mode, the model writes JS that calls `tools.mcp__sprut__execute({code: "…JS…"})`.
- Cloudflare's API server, Cloudflare's portals and the MCP interest group all treat this as a problem to avoid with opt-out switches.

---

## 7. How Agents handle large tool counts natively

**Claude Code** ([MCP docs](https://code.claude.com/docs/en/mcp), [tool search](https://code.claude.com/docs/en/agent-sdk/tool-search))
- Tool search is **on by default**: definitions are withheld and the model searches.
- "Up to five of the most relevant tools are loaded into context by default". They stay until compaction.
- `ENABLE_TOOL_SEARCH` takes unset/`true`, `auto` (activates at 10% of context), `auto:N`, or `false`.
- `alwaysLoad` exempts a tool from deferral.
- "With fewer than ~10 tools … loading everything upfront is typically faster".
- When tool search is on, Claude is told about failed servers.
- Third-party measurement: 7 servers / 131 tools added 843 tokens with deferral vs 46,273 without ([contextbolt](https://contextbolt.com/blog/too-many-mcp-servers/)).

**Anthropic API** ([tool search tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool), [programmatic tool calling](https://platform.claude.com/docs/en/agents-and-tools/tool-use/programmatic-tool-calling))
- Tool search: `tool_search_tool_regex_20251119` / `_bm25_20251119`, `defer_loading: true`, returns `tool_reference` blocks (5 by default). The docs say "selection accuracy degrades once you exceed 30–50 available tools".
- **PTC** is Anthropic's own client-API code mode:
  - Python in a container; tools marked with `allowed_callers: ["code_execution_20260120"]` become async functions that **return strings**.
  - Each nested call pauses execution and returns a `tool_use` with `caller.tool_id` to the API client. The client still executes every call, so it *could* apply approval per call.
  - A pending call times out after about 4 minutes.
  - "Do not rely on `allowed_callers` as a security boundary."

**Codex** ([PR #29486](https://github.com/openai/codex/pull/29486), [OpenAI tool search](https://developers.openai.com/api/docs/guides/tools-tool-search), [config reference](https://learn.chatgpt.com/docs/config-file/config-reference))
- Defers all MCP tools behind `tool_search` when the model supports it (gpt-5.4+).
- At request start the model sees **only namespace or MCP-server names and descriptions**. OpenAI advises "aim to keep each namespace to fewer than 10 functions".
- **Native Code Mode** (`code_mode` feature, `UnderDevelopment`, default off; `code_mode_only` restricts the model to `exec`/`wait`) ([`features/src/lib.rs:149-170,1301-1375`](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/features/src/lib.rs#L1301)).
- `exec` description ([`code-mode-protocol/src/description.rs:23-48`](https://github.com/openai/codex/blob/57a808a3920f05f2d4b9eab2d0493e1df136ae6b/codex-rs/code-mode-protocol/src/description.rs#L23-L48)):
  - "Evaluates the provided JavaScript code in a fresh V8 isolate as an async module."
  - "`await tools.mcp__ologs__get_profile(...)`"
  - "Runs raw JavaScript -- no Node, no file system, no network access, no console."
  - Helpers `text()`, `image()`, `audio()`, `store()`/`load()` (persist across `exec` calls), `notify()`, `yield_control()`, and `ALL_TOOLS` (`{name, description}` for discovery).
  - Yields after 10 s with a `cell_id` that is resumed with `wait`.
- MCP tools are typed as returning the **raw `CallToolResult<T>`**, with `T` from outputSchema, else `unknown` (`description.rs:445-475`).
- Per-server `omit_tools_from = ["direct"|"deferred"|"code_mode"]`.

**Cursor** ([dynamic context discovery](https://cursor.com/blog/dynamic-context-discovery), 2026-01-06)
- Syncs MCP tool descriptions to a folder, **one folder per server**. The agent sees tool names and reads files on demand.
- Cursor chose files over tool search because search "would scatter tools across a flat index".
- A/B test: −46.9% total tokens in runs that called an MCP tool ("statistically significant, with high variance").

**Implication for sprut.**
- sprut is **one MCP server** to every Agent. In Codex's namespace view and Cursor's per-server folders, all Upstreams collapse into a single "sprut" namespace or folder of possibly hundreds of tools, which runs against OpenAI's "<10 per namespace" advice.
- Claude Code's tool search is a flat index across servers, so it is unaffected.
- sprut's server `instructions` and the Upstream prefix in names are the only grouping signals an Agent gets.

---

## Open design questions

1. **Ship code mode at all, and who turns it on?**
   - Options: (a) don't; rely on the Agents' tool search, and on Codex's client-side Code Mode for composition. (b) Opt-in per Agent, e.g. `sprut serve --code-mode` in that Agent's MCP config line. (c) Opt-in per Config. (d) Default on.
   - For (a): Claude Code, Codex and Cursor already defer definitions (§7). Codex is building client-side code mode that keeps approvals (§6.3). The interest group warns about nesting (§4.7).
   - For (b): Cloudflare portals default to `opt_in` precisely so clients can avoid nesting (§2.3). Per-Agent fits ADR-0001, where each Agent runs its own `sprut serve`.
   - For (d): only the Cloudflare portal's early changelog defaulted it on, and its docs later moved the default to opt-in.
2. **Replace the tool list or add to it?**
   - Options: replace (gridctl, Limen, Bifrost, Cloudflare); additive hybrid (ToolHive, pctx sidecar, thehumanworks `on`); hybrid where only writes stay direct (`keepNative`).
   - Evidence: the guided hybrid was best-measured (tmustier). Additive keeps per-tool Agent approvals for direct calls but pays the definition cost, which the Agents' own tool search already mitigates.
3. **Discovery mechanism.**
   - Options: (a) full TS listing in the `execute` description (Cloudflare `createCodeTool`, Docker); (b) a `search` tool returning names, descriptions and TS (gridctl, UTCP, Cloudflare API server, official best-practices three layers); (c) discovery inside the script (`search`/`describe`, `ALL_TOOLS`); (d) none, only an index of Upstreams in `instructions`.
   - Evidence: (a) defeats the definition-size win with many Upstreams. (b) adds a turn: npow found search+execute slowest at 10 tools, and Kun Chen found code mode slowest. (c) saves a turn but adds "tool blindness" risk. Codex uses (c) plus a partial listing.
   - Search algorithm: weighted tokens (Cloudflare), BM25 (thehumanworks, Anthropic), or substring (gridctl; multi-word queries rarely match).
4. **Language: JS only, or TS stripped by esbuild?**
   - JS only: Cloudflare and Codex both say "no TypeScript syntax" and show TS types only as documentation. No transpiler, smaller binary.
   - esbuild: +3.7 MB, and it lowers modern syntax for goja (gridctl does this).
   - pctx type-checks with real TSC, which needs embedding typescript.js.
5. **Engine and isolation boundary** (§5).
   - (a) goja in-process: smallest effort, most Go precedent, no memory cap, process-fatal failure modes.
   - (b) modernc.org/quickjs in-process: built-in memory, time and stack limits, +2.6 MB, less battle-tested.
   - (c) QuickJS on wazero: hard memory isolation; you write your own bindings because fastschema/qjs is unsafe.
   - (d) any of these in a re-exec'd child process: LeanProxy measured 18 ms, and it protects the Upstreams sharing the process.
   - (e) Starlark: Bifrost and ToolHive; not JS, so the model's training advantage is weaker.
6. **Result mapping and types.**
   - Mapping: adopt the converged rule (`isError` throws; structuredContent; JSON text; string) or return raw `CallToolResult` (Codex, Limen).
   - Missing outputSchema (60–70% of tools): `unknown`, `any`, or a generic `CallToolResult`. Optionally sample the first result's shape. Not in any implementation.
   - The official doc suggests an `extract()` helper backed by a small model, which conflicts with sprut having no LLM.
   - Images, audio and resource links: pass through as content blocks or drop.
7. **Approvals** (the decisive Agent-UX question).
   - Options: (a) accept collapse and document it; annotate `execute` `destructiveHint:true`. (b) Code calls only tools marked `readOnlyHint:true` (untrusted hint) or tools allowlisted in the Config; writes stay direct tools. (c) Per-write MRTR elicitation from inside `execute` (pause, elicit, replay; needs a determinism contract like Cloudflare's `codemode.step`). (d) Leave composition to client-side code mode.
   - Evidence: the official doc says a script approval ≠ blanket approval. LeanProxy chose no-go. Cloudflare built (c) with a durable log. Codex's client-side mode keeps approvals natively.
8. **Upstream `input_required` (elicitation, sampling) inside a script.**
   - Options: error like today's direct path; bubble up via MRTR with an integrity-protected `requestState` replay log (spec MUST: HMAC/AEAD, principal binding, TTL); or forbid by policy.
   - Evidence: §4.5. gridctl throws; nothing implements bubbling.
9. **Limits, and how they line up with the Agents.**
   - Script wall time below Codex's 60 s default, or document raising `tool_timeout_sec`.
   - Output cap at or below 6–10k tokens: Cloudflare 6k, Codex 10k, Claude Code warns at 10k.
   - Call-count cap: 32–200 in the wild. Concurrency cap: 4–10. Memory cap (engine-dependent).
   - Cancellation must interrupt the VM and cancel in-flight Upstream calls via ctx.
10. **Concurrency semantics.**
    - Truly async `Promise.all` (Limen, thehumanworks) vs synchronous calls (Docker, gridctl, UTCP).
    - The Upstream side: can a stdio Upstream session take concurrent calls? Codex marks servers `supports_parallel_tool_calls=false` by default.
    - Does the go-sdk ClientSession allow concurrent `CallTool`? Yes (go-sdk v1.8.0): request IDs are atomic, responses are matched by ID, and nothing serializes calls end to end (`internal/jsonrpc2/conn.go:310-335`); stdio only serializes each framed write, Streamable HTTP sends one POST per call. sprut already relies on it (`TestServeForwardsConcurrentCallsIncludingSeveralToOneUpstream`).
11. **State across calls.**
    - Options: stateless per `execute` (Cloudflare API server); `store`/`load` across calls (Codex); a durable log (Cloudflare runtime).
    - The 2026-07-28 spec has no session, so state handles must be explicit ("Stateful Tools" section). sprut has no daemon (ADR-0001), so state dies with the Agent's process.
12. **Nested code-mode Upstreams** (e.g. the Cloudflare API server's own `search`/`execute`).
    - Options: refuse (Cloudflare portals), pass through as ordinary tools, or recommend `?codemode=false` in the Config.
13. **Naming inside the sandbox.**
    - Options: `upstream.tool(args)` namespaces; the flat `upstream__tool` already used on the wire (Anthropic's example uses `google_drive__get_document`; Codex uses `tools.mcp__server__tool`); or `call(upstream, tool, args)`.
    - Upstream names match `^[a-z0-9-]+$` (README), and `-` is not a valid JS identifier. Sanitize (Cloudflare `sanitizeToolName`) or use bracket access.
14. **Observability and audit.** Each inner call should log like a direct call under `-v` (README: "every tool call"). Should the `execute` result include a call log? Cloudflare projects `calls` out of the model's view but keeps them for UIs.
15. **ADR fit.** Code mode is still MCP-only, so it is consistent with ADR-0002, and in-process, so consistent with ADR-0001. A child-process sandbox is still consistent with "no daemon". An embedded JS engine needs an ADR of its own: binary size, cgo-free constraint, and a new security surface.
