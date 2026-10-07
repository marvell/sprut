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
	st := &starter{
		client:   mcp.NewClient(impl, nil),
		env:      env,
		timeout:  startupTimeout,
		starting: &g.starting,
	}

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
			r, err := st.start(ctx, u)
			if err != nil {
				log.Warn("upstream failed; skipped", "upstream", u.Name, "err", err)
				return
			}
			results[i] = r
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
		added := g.addTools(name, r)
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

// addTools serves r's tools as Namespaced tools under name, skipping the
// ones addTool rejects, and returns how many it added.
func (g *Gateway) addTools(name string, r *ready) int {
	added := 0
	for _, tool := range r.tools {
		if err := g.addTool(name, r.session, tool); err != nil {
			g.log.Warn("tool skipped", "upstream", name, "tool", name+separator+tool.Name, "err", err)
			continue
		}
		added++
	}
	return added
}

// ready is an Upstream that is connected and has listed its tools.
type ready struct {
	session *mcp.ClientSession
	tools   []*mcp.Tool
}

// starter starts Upstreams: each as a process with env as its inherited
// environment, given timeout to connect and list its tools.
type starter struct {
	client   *mcp.Client
	env      []string
	timeout  time.Duration
	starting *sync.WaitGroup // the Gateway's, so that Close waits for abandoned starts
}

// start starts u, giving up on it once the timeout runs out.
func (s *starter) start(ctx context.Context, u config.Upstream) (*ready, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
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
	s.starting.Go(func() {
		r, err := s.connect(ctx, u)
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
		res.err = fmt.Errorf("startup timed out after %s", s.timeout)
	}
	return res.r, res.err
}

// connect launches u's process, connects to it and lists its tools.
func (s *starter) connect(ctx context.Context, u config.Upstream) (*ready, error) {
	cmd := exec.Command(u.Command, u.Args...)
	cmd.Env = upstreamEnv(s.env, u.Env)
	session, err := s.client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
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

// upstreamEnv is the environment of an Upstream process: its Config env
// layered on top of the inherited env. exec uses the last value of a
// duplicated key.
func upstreamEnv(env []string, extra map[string]string) []string {
	out := slices.Clone(env)
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		out = append(out, k+"="+extra[k])
	}
	return out
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
	log := g.log.With("upstream", upstream, "tool", namespaced.Name)
	g.server.AddTool(&namespaced, forward(session, original, log))
	log.Debug("tool added")
	return nil
}

// forward handles calls to a Namespaced tool by calling the Upstream's tool
// original over session, and logs each call.
func forward(session *mcp.ClientSession, original string, log *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		begin := time.Now()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: original, Arguments: req.Params.Arguments})
		attrs := []any{"duration", time.Since(begin), "isError", res != nil && res.IsError}
		if err != nil {
			attrs = append(attrs, "err", err)
		}
		log.Debug("tool called", attrs...)
		if res != nil {
			// serverInfo identifies the responder of each hop; drop the
			// Upstream's so the Gateway's own is reported to the Agent.
			delete(res.Meta, mcp.MetaKeyServerInfo)
		}
		return res, err
	}
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
