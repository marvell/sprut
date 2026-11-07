package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/audit"
	"github.com/marvell/sprut/internal/sandbox"
)

// executeTool is the tool of Code mode. It never collides with a
// Namespaced tool, whose name always holds the separator.
const executeTool = "execute"

// ScriptTimeLimit is how long a Script may run. It stays below Codex's
// 60-second tool timeout, so that the Gateway reports a timeout, not the
// Agent.
const ScriptTimeLimit = 50 * time.Second

// CodeMode turns on Code mode: the Gateway also serves the execute tool.
type CodeMode struct {
	// ScriptTimeLimit is how long a Script may run: the constant of that
	// name, except in tests.
	ScriptTimeLimit time.Duration
}

// The other limits of a Script.
const (
	scriptMemory    = 64 << 20 // bytes
	scriptCallDepth = 5000     // the engine's stack slots, about one per call
	scriptOutput    = 40000    // characters
	scriptInFlight  = 8        // calls running at once
	scriptCalls     = 100      // calls in all
)

// limits bound every Script.
func (c *CodeMode) limits() sandbox.Limits {
	return sandbox.Limits{Time: c.ScriptTimeLimit, Memory: scriptMemory, Stack: scriptCallDepth, Output: scriptOutput, InFlight: scriptInFlight, Calls: scriptCalls}
}

// executeDescription is the Script contract, as the Agent reads it,
// followed by the tools that a Script may call.
func executeDescription(limits sandbox.Limits, tools catalog) string {
	return fmt.Sprintf(`Run a JavaScript script that calls read-only tools and gives back only what it prints and returns: chain calls, fan out over many items, or cut a large result down, so that intermediate results stay out of your context.

The script is the body of an async function, so top-level await works. Write plain JavaScript (ES2023), without TypeScript annotations. The only globals beyond standard JavaScript are call and console.

await call("<upstream>__<tool>", args) calls a tool by its name in the list below, with args as its arguments. Your client may show the same tool with a prefix of its own, such as mcp__sprut__; leave the prefix out. call returns a Promise of:
- the result's structuredContent, if it has one;
- else, when its content is one text block, the JSON value of that text, or the text as a string if it isn't JSON;
- else its content array.
When you don't know a result's shape, print a slice of it first.
A result with isError throws an Error whose message is the result's text; catch it with try/catch.
Up to %d calls run at once, so Promise.all over calls runs them in parallel; the others wait their turn. A script may make at most %d calls: the next one throws an Error.

A script can call only tools annotated readOnlyHint: true. Calling any other tool throws an Error: call it directly instead, outside the script.

console.log, console.error and console.warn print to one output: strings as they are, other values as JSON. A return value other than undefined follows as JSON on a line of its own.

Each script starts in a fresh VM, so nothing persists between scripts.

A script that fails to parse, throws or runs past a limit gives an error result: the message with its line, then the output printed so far. Limits: %s of wall time, %d MB of memory and %d characters of output, past which the output is cut with a line saying how much.

%s`,
		limits.InFlight, limits.Calls, limits.Time, limits.Memory>>20, limits.Output, tools.scriptTools())
}

// scriptTools lists the tools a Script may call, by Upstream: names only,
// as the Agent takes everything else from the Namespaced tools.
func (c catalog) scriptTools() string {
	byUpstream := map[string][]string{}
	for name, t := range c {
		if t.readOnly {
			byUpstream[t.upstream] = append(byUpstream[t.upstream], name)
		}
	}
	if len(byUpstream) == 0 {
		return "No tool can be called from a script: no upstream marks one readOnlyHint: true.\n"
	}
	var b strings.Builder
	b.WriteString("Tools a script can call, by upstream; their input schemas are on the tools of the same name in your tool list:\n")
	for _, upstream := range slices.Sorted(maps.Keys(byUpstream)) {
		names := byUpstream[upstream]
		slices.Sort(names)
		fmt.Fprintf(&b, "- %s: %s\n", upstream, strings.Join(names, ", "))
	}
	return b.String()
}

// addExecute serves the execute tool. Its description lists the Namespaced
// tools a Script may call, so it is added once they all are.
func (g *Gateway) addExecute(c *CodeMode) {
	limits := c.limits()
	g.server.AddTool(&mcp.Tool{
		Name:        executeTool,
		Description: executeDescription(limits, g.catalog),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{"type": "string", "description": "the script: the body of an async function"},
			},
			"required": []any{"code"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, g.execute(limits, g.log.With("tool", executeTool)))
	g.codeMode = true
}

// execute handles calls to the execute tool by running their Script, whose
// calls go to the catalog, and logs each call with its outcome, to log and
// to the Audit log.
func (g *Gateway) execute(limits sandbox.Limits, log *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		begin := time.Now()
		run := &scriptRun{catalog: g.catalog, n: int(g.scripts.Add(1))}
		var in struct {
			Code *string `json:"code"`
		}
		var res *mcp.CallToolResult
		var err error
		if jerr := json.Unmarshal(req.Params.Arguments, &in); jerr != nil || in.Code == nil {
			in.Code, res = new(""), errorResult(errors.New(`execute takes {"code": string}`))
		} else {
			res, err = runScript(ctx, *in.Code, run.call, limits)
		}
		took := time.Since(begin)
		logCall(log, "script executed", took, res, err)
		if g.audit != nil {
			outcome, why := outcome(ctx, res, err)
			g.audit.Script(audit.Script{N: run.n, Took: took, Outcome: outcome, Err: why,
				Calls: int(run.calls.Load()), Code: *in.Code, ResultBytes: size(res)})
		}
		return res, err
	}
}

// runScript runs the Script code under limits, its calls going to caller.
// A cancelled Script ends in ctx's error, as a cancelled direct call does;
// any other failure is an isError result.
func runScript(ctx context.Context, code string, caller sandbox.Caller, limits sandbox.Limits) (*mcp.CallToolResult, error) {
	// The sandbox formats the result, a failed Script's included.
	out, err := sandbox.Run(ctx, code, caller, limits)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil && !errors.As(err, new(*sandbox.Error)):
		return errorResult(err), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}, IsError: err != nil}, nil
}

// scriptRun is one Script being run: its number, and the calls it has made
// to Upstreams.
type scriptRun struct {
	catalog catalog
	n       int
	calls   atomic.Int64
}

// call calls the Namespaced tool name with args for the Script, by the
// path a direct call takes, and returns the JSON of the value the Script
// gets. Only a read-only tool may be called this way: a Script runs
// without the Agent approving each call, and a write must have that
// approval. A result with isError is an error saying its text.
func (r *scriptRun) call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	t := r.catalog[name]
	switch {
	case t == nil:
		return nil, fmt.Errorf("unknown tool %q: call takes a name listed in execute's description, without any prefix your client adds; a tool not listed there is not read-only, so call it directly", name)
	case !t.readOnly:
		return nil, fmt.Errorf("tool %q is not read-only, so a script can't call it; call it directly", name)
	}
	r.calls.Add(1)
	res, err := t.call(ctx, args, r)
	if err != nil {
		return nil, err
	}
	return scriptValue(name, res)
}

// scriptValue is the JSON of the value a Script gets from res: the first
// of its structuredContent, the JSON of its only content block, when that
// is text that parses as JSON, that text as a string, or its content.
// A result with isError is an error whose message is its text, or one
// naming tool if it has none.
func scriptValue(tool string, res *mcp.CallToolResult) (json.RawMessage, error) {
	if res.IsError {
		if t := text(res); t != "" {
			return nil, errors.New(t)
		}
		return nil, fmt.Errorf("tool %q returned an error", tool)
	}
	if res.StructuredContent != nil {
		return json.Marshal(res.StructuredContent)
	}
	if len(res.Content) == 1 {
		if t, ok := res.Content[0].(*mcp.TextContent); ok {
			if json.Valid([]byte(t.Text)) {
				return json.RawMessage(t.Text), nil
			}
			return json.Marshal(t.Text)
		}
	}
	if res.Content == nil {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(res.Content)
}

// text is the text of res's text blocks, one per line.
func text(res *mcp.CallToolResult) string {
	var texts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, t.Text)
		}
	}
	return strings.Join(texts, "\n")
}
