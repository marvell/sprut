package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
)

// limits bound every Script.
func (c *CodeMode) limits() sandbox.Limits {
	return sandbox.Limits{Time: c.ScriptTimeLimit, Memory: scriptMemory, Stack: scriptCallDepth, Output: scriptOutput}
}

// executeDescription is the Script contract, as the Agent reads it.
func executeDescription(limits sandbox.Limits) string {
	return fmt.Sprintf(`Run a JavaScript script in a sandbox and get back only what it prints and returns.

The script is the body of an async function, so top-level await works. Write plain JavaScript (ES2023), without TypeScript annotations. The only global beyond standard JavaScript is console.

console.log, console.error and console.warn print to one output: strings as they are, other values as JSON. A return value other than undefined follows as JSON on a line of its own.

Each script starts in a fresh VM, so nothing persists between calls.

A script that fails to parse, throws or runs past a limit gives an error result: the message with its line, then the output printed so far. Limits: %s of wall time, %d MB of memory, a call depth of about %d, and %d characters of output, past which the output is cut with a line saying how much.`,
		limits.Time, limits.Memory>>20, limits.Stack, limits.Output)
}

// addExecute serves the execute tool.
func (g *Gateway) addExecute(c *CodeMode) {
	limits := c.limits()
	g.server.AddTool(&mcp.Tool{
		Name:        executeTool,
		Description: executeDescription(limits),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{"type": "string", "description": "the script: the body of an async function"},
			},
			"required": []any{"code"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, execute(limits, g.log.With("tool", executeTool)))
	g.tools[executeTool] = true
}

// execute handles calls to the execute tool by running their Script, and
// logs each call with its outcome.
func execute(limits sandbox.Limits, log *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		begin := time.Now()
		res, err := runScript(ctx, req.Params.Arguments, limits)
		logCall(log, "script executed", time.Since(begin), res, err)
		return res, err
	}
}

// runScript runs the Script in args under limits. A cancelled Script ends
// in ctx's error, as a cancelled direct call does; any other failure is an
// isError result.
func runScript(ctx context.Context, args json.RawMessage, limits sandbox.Limits) (*mcp.CallToolResult, error) {
	var in struct {
		Code *string `json:"code"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == nil {
		return errorResult(errors.New(`execute takes {"code": string}`)), nil
	}
	// The sandbox formats the result, a failed Script's included.
	out, err := sandbox.Run(ctx, *in.Code, limits)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil && !errors.As(err, new(*sandbox.Error)):
		return errorResult(err), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}, IsError: err != nil}, nil
}
