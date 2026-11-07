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

// scriptLimits bound every Script. Its time limit stays below Codex's
// 60-second tool timeout, so that the Gateway reports a timeout, not the
// Agent.
var scriptLimits = sandbox.Limits{Time: 50 * time.Second, Memory: 64 << 20}

// executeDescription is the Script contract, as the Agent reads it.
var executeDescription = fmt.Sprintf(`Run a JavaScript script in a sandbox and get back only what it prints and returns.

The script is the body of an async function, so top-level await works. Write plain JavaScript (ES2023), without TypeScript annotations. The only global beyond standard JavaScript is console.

console.log, console.error and console.warn print to one output: strings as they are, other values as JSON. A return value other than undefined follows as JSON on a line of its own.

Each script starts in a fresh VM, so nothing persists between calls.

A script that fails to parse, throws or runs past a limit gives an error result: the message with its line, then the output printed so far. Limits: %s of wall time, %d MB of memory.`,
	scriptLimits.Time, scriptLimits.Memory>>20)

// addExecute serves the execute tool.
func (g *Gateway) addExecute() {
	g.server.AddTool(&mcp.Tool{
		Name:        executeTool,
		Description: executeDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{"type": "string", "description": "the script: the body of an async function"},
			},
			"required": []any{"code"},
		},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, execute(g.log.With("tool", executeTool)))
	g.tools[executeTool] = true
}

// execute handles calls to the execute tool by running their Script, and
// logs each call with its outcome.
func execute(log *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		begin := time.Now()
		res, err := runScript(ctx, req.Params.Arguments)
		attrs := []any{"duration", time.Since(begin)}
		if err != nil {
			attrs = append(attrs, "err", err)
		}
		log.Debug("script executed", append(attrs, "isError", res != nil && res.IsError)...)
		return res, err
	}
}

// runScript runs the Script in args. A cancelled Script ends in ctx's
// error, as a cancelled direct call does; any other failure is an isError
// result.
func runScript(ctx context.Context, args json.RawMessage) (*mcp.CallToolResult, error) {
	var in struct {
		Code *string `json:"code"`
	}
	if err := json.Unmarshal(args, &in); err != nil || in.Code == nil {
		return errorResult(errors.New(`execute takes {"code": string}`)), nil
	}
	out, err := sandbox.Run(ctx, *in.Code, scriptLimits)
	var scriptErr *sandbox.Error
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.As(err, &scriptErr):
		text := scriptErr.Message
		if scriptErr.Output != "" {
			text += "\n" + scriptErr.Output
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}, IsError: true}, nil
	case err != nil:
		return errorResult(err), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out}}}, nil
}
