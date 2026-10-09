// Package mcptest provides fake Upstreams for tests: a stdio Upstream that
// is the test binary re-executing itself, and HTTP Upstreams on httptest
// servers. Each offers the tools echo and fail, plus whatever its spec adds.
package mcptest

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Protocol versions of the two eras: legacy before 2026-07-28, modern from
// it.
const (
	Legacy = "2025-06-18"
	Modern = "2026-07-28"
)

// The fake Upstreams' tools, as raw JSON so that tests can assert that the
// Gateway passes them through unchanged.
var (
	// ObjectSchema is the input schema of a tool that takes no arguments.
	ObjectSchema    = map[string]any{"type": "object"}
	EchoInputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "text to echo"},
		},
		"required": []any{"text"},
	}
	EchoOutputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"echoed": map[string]any{"type": "string"},
		},
	}
	echoTool = &mcp.Tool{
		Name:         "echo",
		Title:        "Echo",
		Description:  "Echoes its text argument back.",
		InputSchema:  EchoInputSchema,
		OutputSchema: EchoOutputSchema,
	}
	failTool = &mcp.Tool{
		Name:        "fail",
		Description: "Always returns an isError result.",
		InputSchema: ObjectSchema,
	}
)

// EchoResult is a fake Upstream's answer to echo with text, as JSON.
func EchoResult(text string) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": "echo: " + text}},
		"structuredContent": map[string]any{"echoed": text},
		"_meta":             map[string]any{"fake/trace": "abc"},
	}
}

// NewServer is the MCP server of a fake Upstream, stdio or HTTP, offering
// echo and fail. A non-empty protocol is the one protocol version it
// supports, which sets its era.
func NewServer(protocol string) *mcp.Server {
	var opts mcp.ServerOptions
	if protocol != "" {
		opts.SupportedProtocolVersions = []string{protocol}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-upstream", Version: "0.0.1"}, &opts)
	server.AddTool(echoTool, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Text string }
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: "echo: " + args.Text}},
			StructuredContent: map[string]any{"echoed": args.Text},
			Meta:              mcp.Meta{"fake/trace": "abc"},
		}, nil
	})
	server.AddTool(failTool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "it failed"}},
			IsError: true,
		}, nil
	})
	return server
}
