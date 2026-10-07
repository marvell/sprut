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

// Gateway is a started set of Upstreams and the MCP server exposing their tools.
type Gateway struct {
	server   *mcp.Server
	sessions []*mcp.ClientSession // one per started Upstream
	starting sync.WaitGroup       // Upstream starts, including abandoned ones
	failed   int                  // Upstreams that failed or timed out at startup
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

	results := make([]*ready, len(upstreams))
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
			results[i] = s
		})
	}
	wg.Wait()

	// Tools are added in Config order, so the tool list doesn't depend on
	// which Upstream was quickest.
	tools := 0
	for i, r := range results {
		if r == nil {
			continue
		}
		name := upstreams[i].Name
		g.sessions = append(g.sessions, r.session)
		added := 0
		for _, tool := range r.tools {
			if err := g.addTool(name, r.session, tool); err != nil {
				log.Warn("tool skipped", "upstream", name, "tool", name+separator+tool.Name, "err", err)
				continue
			}
			added++
		}
		log.Info("upstream ready", "upstream", name, "tools", added,
			"protocol", r.session.InitializeResult().ProtocolVersion)
		tools += added
	}
	g.failed = attempts - len(g.sessions)
	log.Info("gateway started", "ready", len(g.sessions), "failed", g.failed, "tools", tools)
	return g
}

// Failed reports how many Upstreams failed or timed out at startup.
func (g *Gateway) Failed() int { return g.failed }

// ready is an Upstream that is connected and has listed its tools.
type ready struct {
	session *mcp.ClientSession
	tools   []*mcp.Tool
}

func (g *Gateway) startUpstream(ctx context.Context, client *mcp.Client, u config.Upstream, env []string, timeout time.Duration) (*ready, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Connecting runs on its own, because a failed connect stops the
	// Upstream before returning, and the SDK gives a hung Upstream seconds of
	// grace to exit. Startup gives up at the deadline instead; Close waits
	// for the stopping to finish.
	type result struct {
		r   *ready
		err error
	}
	done := make(chan result) // unbuffered: a result is either taken or left
	g.starting.Go(func() {
		r, err := g.connectUpstream(ctx, client, u, env)
		select {
		case done <- result{r, err}:
		case <-ctx.Done(): // given up on
			if r != nil {
				_ = r.session.Close()
			}
		}
	})

	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		res.err = ctx.Err()
	}
	if res.err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.err = fmt.Errorf("startup timed out after %s", timeout)
	}
	return res.r, res.err
}

func (g *Gateway) connectUpstream(ctx context.Context, client *mcp.Client, u config.Upstream, env []string) (*ready, error) {
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

	r := &ready{session: session}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			_ = session.Close()
			return nil, fmt.Errorf("listing tools: %w", err)
		}
		r.tools = append(r.tools, tool)
	}
	return r, nil
}

// addTool serves tool as a Namespaced tool. A tool that Agents or the SDK
// would reject is not added, and the error says why.
func (g *Gateway) addTool(upstream string, session *mcp.ClientSession, tool *mcp.Tool) (err error) {
	original := tool.Name
	namespaced := *tool
	namespaced.Name = upstream + separator + original
	if !validToolName.MatchString(namespaced.Name) {
		return fmt.Errorf("name must match %s", validToolName)
	}
	// AddTool panics on a tool it finds invalid, such as one without an
	// object input schema, before adding anything.
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	g.server.AddTool(&namespaced, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		begin := time.Now()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: original, Arguments: req.Params.Arguments})
		attrs := []any{"upstream", upstream, "tool", namespaced.Name, "duration", time.Since(begin),
			"isError", res != nil && res.IsError}
		if err != nil {
			attrs = append(attrs, "err", err)
		}
		g.log.Debug("tool called", attrs...)
		if res != nil {
			// serverInfo identifies the responder of each hop; drop the
			// Upstream's so the Gateway's own is reported to the Agent.
			delete(res.Meta, mcp.MetaKeyServerInfo)
		}
		return res, err
	})
	g.log.Debug("tool added", "upstream", upstream, "tool", namespaced.Name)
	return nil
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
