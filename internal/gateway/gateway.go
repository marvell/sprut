// Package gateway connects to Upstreams and serves their tools to an Agent
// as Namespaced tools.
package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/config"
)

// separator joins an Upstream's name and a tool's name into a Namespaced tool.
const separator = "__"

// Gateway is a started set of Upstreams and the MCP server exposing their tools.
type Gateway struct {
	impl     *mcp.Implementation
	server   *mcp.Server
	sessions []*mcp.ClientSession // one per started Upstream
	log      *slog.Logger
}

// Start launches every Upstream, collects their tools and returns a Gateway
// ready to serve them. env is the environment Upstream processes inherit.
func Start(ctx context.Context, upstreams []config.Upstream, env []string, version string, log *slog.Logger) (*Gateway, error) {
	impl := &mcp.Implementation{Name: "sprut", Version: version}
	g := &Gateway{
		impl: impl,
		server: mcp.NewServer(impl, &mcp.ServerOptions{
			// Advertise only tools; the tool list is fixed for the session.
			Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		}),
		log: log,
	}
	for _, u := range upstreams {
		if err := g.startUpstream(ctx, u, env); err != nil {
			g.Close()
			return nil, fmt.Errorf("upstream %s: %w", u.Name, err)
		}
	}
	return g, nil
}

func (g *Gateway) startUpstream(ctx context.Context, u config.Upstream, env []string) error {
	cmd := exec.Command(u.Command, u.Args...)
	cmd.Env = env
	client := mcp.NewClient(g.impl, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return err
	}
	g.sessions = append(g.sessions, session)

	count := 0
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("listing tools: %w", err)
		}
		g.addTool(u.Name, session, tool)
		count++
	}
	g.log.Info("upstream ready", "upstream", u.Name, "tools", count,
		"protocol", session.InitializeResult().ProtocolVersion)
	return nil
}

func (g *Gateway) addTool(upstream string, session *mcp.ClientSession, tool *mcp.Tool) {
	original := tool.Name
	namespaced := *tool
	namespaced.Name = upstream + separator + original
	g.server.AddTool(&namespaced, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: original, Arguments: req.Params.Arguments})
		if res != nil {
			// serverInfo identifies the responder of each hop; drop the
			// Upstream's so the Gateway's own is reported to the Agent.
			delete(res.Meta, mcp.MetaKeyServerInfo)
		}
		return res, err
	})
	g.log.Debug("tool", "name", namespaced.Name)
}

// Serve serves MCP to one Agent over stdin/stdout until the Agent
// disconnects or ctx is done.
func (g *Gateway) Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	return g.server.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}})
}

// Close shuts every Upstream down.
func (g *Gateway) Close() {
	for _, s := range g.sessions {
		s.Close()
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
