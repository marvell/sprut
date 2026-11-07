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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	return fmt.Sprintf(`Compose calls to read-only tools in a JavaScript script, so that their results stay out of your context: get back only what the script prints and returns.

The script is the body of an async function, so top-level await works. Write plain JavaScript (ES2023), without TypeScript annotations. The only globals beyond standard JavaScript are call and console.

await call("<upstream>__<tool>", args) calls a tool by the name you see it under, with args as its arguments, and returns a Promise of:
- the result's structuredContent, if it has one;
- else, when its content is one text block, the JSON value of that text, or the text as a string if it isn't JSON;
- else its content array.
A result with isError throws an Error whose message is the result's text; catch it with try/catch.
Up to %d calls run at once, so Promise.all over calls runs them in parallel; the others wait their turn. A script may make at most %d calls: the next one throws an Error.

A script can call only tools annotated readOnlyHint: true. Calling any other tool throws an Error: call it directly instead, outside the script.

console.log, console.error and console.warn print to one output: strings as they are, other values as JSON. A return value other than undefined follows as JSON on a line of its own.

Each script starts in a fresh VM, so nothing persists between scripts.

A script that fails to parse, throws or runs past a limit gives an error result: the message with its line, then the output printed so far. Limits: %s of wall time, %d MB of memory, a call depth of about %d, and %d characters of output, past which the output is cut with a line saying how much.

%s`,
		limits.InFlight, limits.Calls, limits.Time, limits.Memory>>20, limits.Stack, limits.Output, tools.scriptTools())
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
	b.WriteString("Tools a script can call, by upstream, named exactly as in your tool list, where their input schemas are:\n")
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
	}, execute(limits, g.catalog.callFromScript, g.log.With("tool", executeTool)))
	g.codeMode = true
}

// execute handles calls to the execute tool by running their Script, whose
// calls go to caller, and logs each call with its outcome.
func execute(limits sandbox.Limits, caller sandbox.Caller, log *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		begin := time.Now()
		res, err := runScript(ctx, req.Params.Arguments, caller, limits)
		logCall(log, "script executed", time.Since(begin), res, err)
		return res, err
	}
}

// runScript runs the Script in args under limits, its calls going to
// caller. A cancelled Script ends in ctx's error, as a cancelled direct
// call does; any other failure is an isError result.
func runScript(ctx context.Context, args json.RawMessage, caller sandbox.Caller, limits sandbox.Limits) (*mcp.CallToolResult, error) {
	var in struct {
		Code *string `json:"code"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == nil {
		return errorResult(errors.New(`execute takes {"code": string}`)), nil
	}
	// The sandbox formats the result, a failed Script's included.
	out, err := sandbox.Run(ctx, *in.Code, caller, limits)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil && !errors.As(err, new(*sandbox.Error)):
		return errorResult(err), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}, IsError: err != nil}, nil
}

// callFromScript calls the Namespaced tool name with args for a Script, by
// the path a direct call takes, and returns the JSON of the value the
// Script gets. Only a read-only tool may be called this way: a Script runs
// without the Agent approving each call, and a write must have that
// approval. A result with isError is an error saying its text.
func (c catalog) callFromScript(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	t := c[name]
	switch {
	case t == nil:
		return nil, fmt.Errorf("unknown tool %q: a script can call only read-only tools, and any other must be called directly", name)
	case !t.readOnly:
		return nil, fmt.Errorf("tool %q is not read-only, so a script can't call it; call it directly", name)
	}
	res, err := t.call(ctx, args, t.scriptLog)
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
