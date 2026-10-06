// Package gateway connects to Upstreams and serves their tools to an Agent
// as Namespaced tools.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/config"
)

// separator joins an Upstream's name and a tool's name into a Namespaced tool.
const separator = "__"

// validToolName is what Agents accept as a tool name. A Namespaced tool that
// doesn't match is skipped, never truncated or renamed.
var validToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func namespace(upstream, tool string) string {
	return upstream + separator + tool
}

// Gateway is a started set of Upstreams and the MCP server exposing their tools.
type Gateway struct {
	server   *mcp.Server
	sessions []*mcp.ClientSession // one per started Upstream
	starting sync.WaitGroup       // Upstream starts, including abandoned ones
	log      *slog.Logger
}

// Start launches every Upstream concurrently, collects their tools and
// returns a Gateway ready to serve them. env is the environment Upstream
// processes inherit. Each Upstream gets startupTimeout to connect and list
// its tools; one that fails or runs out of time is logged and left out.
func Start(ctx context.Context, upstreams []config.Upstream, env []string, startupTimeout time.Duration, version string, log *slog.Logger) *Gateway {
	impl := &mcp.Implementation{Name: "sprut", Version: version}
	g := &Gateway{
		server: mcp.NewServer(impl, &mcp.ServerOptions{
			// Advertise only tools; the tool list is fixed for the session.
			Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		}),
		log: log,
	}
	client := mcp.NewClient(impl, nil)

	started := make([]*started, len(upstreams))
	var (
		wg       sync.WaitGroup
		attempts int
	)
	for i, u := range upstreams {
		if u.Transport != config.Stdio {
			log.Warn("HTTP transport is not supported yet; upstream skipped", "upstream", u.Name)
			continue
		}
		attempts++
		wg.Go(func() {
			s, err := g.startUpstream(ctx, client, u, env, startupTimeout)
			if err != nil {
				log.Warn("upstream failed; skipped", "upstream", u.Name, "err", err)
				return
			}
			started[i] = s
		})
	}
	wg.Wait()

	// Tools are added in Config order, so the tool list doesn't depend on
	// which Upstream was quickest.
	tools := 0
	for i, s := range started {
		if s == nil {
			continue
		}
		g.sessions = append(g.sessions, s.session)
		for _, tool := range s.tools {
			g.addTool(upstreams[i].Name, s.session, tool)
		}
		tools += len(s.tools)
	}
	log.Info("gateway started", "ready", len(g.sessions), "failed", attempts-len(g.sessions), "tools", tools)
	return g
}

// started is an Upstream that is connected and has listed its tools.
type started struct {
	session *mcp.ClientSession
	tools   []*mcp.Tool
}

func (g *Gateway) startUpstream(ctx context.Context, client *mcp.Client, u config.Upstream, env []string, timeout time.Duration) (*started, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Connecting runs on its own, because a failed connect stops the
	// Upstream before returning, and the SDK gives a hung Upstream seconds of
	// grace to exit. Startup gives up at the deadline instead; Close waits
	// for the stopping to finish.
	type result struct {
		s   *started
		err error
	}
	done := make(chan result) // unbuffered: a result is either taken or left
	g.starting.Go(func() {
		s, err := g.connectUpstream(ctx, client, u, env)
		select {
		case done <- result{s, err}:
		case <-ctx.Done(): // given up on
			if s != nil {
				_ = s.session.Close()
			}
		}
	})

	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		r.err = ctx.Err()
	}
	if r.err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		r.err = fmt.Errorf("startup timed out after %s", timeout)
	}
	return r.s, r.err
}

func (g *Gateway) connectUpstream(ctx context.Context, client *mcp.Client, u config.Upstream, env []string) (*started, error) {
	cmd := exec.Command(u.Command, u.Args...)
	// Config env is layered on top of the inherited environment: exec uses
	// the last value of a duplicated key.
	cmd.Env = slices.Clone(env)
	for _, k := range slices.Sorted(maps.Keys(u.Env)) {
		cmd.Env = append(cmd.Env, k+"="+u.Env[k])
	}
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, err
	}

	s := &started{session: session}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			_ = session.Close()
			return nil, fmt.Errorf("listing tools: %w", err)
		}
		if name := namespace(u.Name, tool.Name); !validToolName.MatchString(name) {
			g.log.Warn("tool name not accepted by agents; tool skipped", "upstream", u.Name,
				"tool", name, "want", validToolName)
			continue
		}
		s.tools = append(s.tools, tool)
	}
	g.log.Info("upstream ready", "upstream", u.Name, "tools", len(s.tools),
		"protocol", session.InitializeResult().ProtocolVersion)
	return s, nil
}

func (g *Gateway) addTool(upstream string, session *mcp.ClientSession, tool *mcp.Tool) {
	original := tool.Name
	namespaced := *tool
	namespaced.Name = namespace(upstream, original)
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

// Close shuts every Upstream down, including those still being stopped after
// failing to start.
func (g *Gateway) Close() {
	for _, s := range g.sessions {
		// An Upstream stopped by a signal reports an error; shutdown goes on.
		_ = s.Close()
	}
	g.starting.Wait()
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
