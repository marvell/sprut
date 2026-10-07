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
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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
	server      *mcp.Server
	sessions    []*mcp.ClientSession // one per started Upstream
	starting    sync.WaitGroup       // Upstream starts, including abandoned ones
	failed      int                  // Upstreams that failed or timed out at startup
	tools       map[string]bool      // Namespaced tools served; fixed once Start returns
	agentLogged atomic.Bool          // whether "agent connected" is logged
	log         *slog.Logger
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
		tools: map[string]bool{},
		log:   log,
	}
	g.server.AddReceivingMiddleware(g.answerUnknownTools, g.logAgentConnected)
	st := &starter{
		// The Gateway proxies no interactive input, so a modern Upstream's
		// request for it comes back to forward rather than being retried.
		client:   mcp.NewClient(impl, &mcp.ClientOptions{MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}}),
		env:      env,
		timeout:  startupTimeout,
		starting: &g.starting,
		log:      log,
	}

	results := make([]*ready, len(upstreams))
	var wg sync.WaitGroup
	for i, u := range upstreams {
		wg.Go(func() {
			begin := time.Now()
			r, err := st.start(ctx, u)
			if err != nil {
				log.Warn("upstream failed; skipped", "upstream", u.Name, "err", err)
				return
			}
			r.took = time.Since(begin)
			results[i] = r
		})
	}
	wg.Wait()

	// Upstreams are logged in Config order, so the log doesn't depend on
	// which Upstream was quickest. (The SDK lists tools by name anyway.)
	tools := 0
	for i, r := range results {
		if r == nil {
			continue
		}
		name := upstreams[i].Name
		g.sessions = append(g.sessions, r.session)
		added := g.addTools(name, r)
		log.Info("upstream ready", "upstream", name, "tools", added,
			"protocol", r.session.InitializeResult().ProtocolVersion, "duration", r.took)
		tools += added
	}
	g.failed = len(upstreams) - len(g.sessions)
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
	took    time.Duration // to start, which "upstream ready" reports, as it is logged once all are done
}

// starter starts Upstreams, giving each timeout to connect and list its
// tools. env is the environment sprut was given: a stdio Upstream's process
// inherits it, and an HTTP Upstream's proxy comes from it.
type starter struct {
	client   *mcp.Client
	env      []string
	timeout  time.Duration
	starting *sync.WaitGroup // the Gateway's, so that Close waits for abandoned starts
	log      *slog.Logger
}

// start starts u, giving up on it once the timeout runs out.
func (s *starter) start(ctx context.Context, u config.Upstream) (*ready, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	// Connecting runs on its own, because a failed connect stops the
	// Upstream before returning, and a hung Upstream gets seconds of grace
	// to exit. Startup gives up at the deadline instead; Close waits
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

// connect connects to u, launching its process if it is a stdio Upstream,
// and lists its tools.
func (s *starter) connect(ctx context.Context, u config.Upstream) (*ready, error) {
	var transport mcp.Transport
	switch u.Transport {
	case config.Stdio:
		cmd := exec.Command(u.Command, u.Args...)
		cmd.Env = upstreamEnv(s.env, u.Env)
		transport = &stdioTransport{cmd: cmd, log: s.log.With("upstream", u.Name)}
	case config.HTTP:
		var err error
		if transport, err = newHTTPTransport(u, s.env); err != nil {
			return nil, err
		}
	}
	session, err := s.client.Connect(ctx, transport, nil)
	if err != nil {
		// The SDK never falls back to the deprecated HTTP+SSE transport, and
		// an SSE-only server fails here with a bare HTTP status, so say why.
		if t, ok := transport.(*mcp.StreamableClientTransport); ok && sseOnly(ctx, t) {
			err = fmt.Errorf("server speaks only the deprecated HTTP+SSE transport, which is not supported: %w", err)
		}
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
	if stdio, ok := transport.(*stdioTransport); ok {
		stdio.started()
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
	g.server.AddTool(&namespaced, forward(upstream, session, original, log))
	g.tools[namespaced.Name] = true
	log.Debug("tool added")
	return nil
}

// answerUnknownTools answers a call to a tool the Gateway doesn't serve with
// an isError result, where the SDK would answer with a protocol error.
func (g *Gateway) answerUnknownTools(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if call, ok := req.(*mcp.CallToolRequest); ok && !g.tools[call.Params.Name] {
			return errorResult(fmt.Errorf("unknown tool %q", call.Params.Name)), nil
		}
		return next(ctx, method, req)
	}
}

// logAgentConnected logs, once, the protocol version the Agent speaks, as
// soon as the SDK has settled it: by the initialize handshake for a legacy
// Agent, or from the first request of a modern one, even one that fails.
func (g *Gateway) logAgentConnected(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if g.agentLogged.Load() {
			return res, err
		}
		var protocol string
		if init, ok := res.(*mcp.InitializeResult); ok {
			protocol = init.ProtocolVersion // negotiated, unlike the one asked for
		} else if ss, ok := req.GetSession().(*mcp.ServerSession); ok {
			if p := ss.InitializeParams(); p != nil {
				protocol = p.ProtocolVersion
			}
		}
		if protocol != "" && g.agentLogged.CompareAndSwap(false, true) {
			g.log.Info("agent connected", "protocol", protocol)
		}
		return res, err
	}
}

// forward handles calls to a Namespaced tool by calling the Upstream's tool
// original over session, and logs each call. A call the Upstream doesn't
// answer, such as one to an Upstream that is gone, ends in an isError result
// naming it, and so does one where it asks for interactive input.
func forward(upstream string, session *mcp.ClientSession, original string, log *slog.Logger) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		begin := time.Now()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: original, Arguments: req.Params.Arguments})
		attrs := []any{"duration", time.Since(begin)}
		if err != nil {
			attrs = append(attrs, "err", err)
		}
		switch {
		case res == nil:
		case res.NeedsInput():
			res = errorResult(fmt.Errorf("upstream %q asked for input mid-call, but interactive input is not supported by the gateway", upstream))
		default:
			// serverInfo identifies the responder of each hop; drop the
			// Upstream's so the Gateway's own is reported to the Agent.
			delete(res.Meta, mcp.MetaKeyServerInfo)
		}
		if err != nil && ctx.Err() == nil && !answered(err) {
			res, err = errorResult(fmt.Errorf("call to upstream %q failed: %w", upstream, err)), nil
		}
		log.Debug("tool called", append(attrs, "isError", res != nil && res.IsError)...)
		return res, err
	}
}

// errorResult is an isError result saying err.
func errorResult(err error) *mcp.CallToolResult {
	res := &mcp.CallToolResult{}
	res.SetError(err)
	return res
}

// answered reports whether err is the Upstream's own JSON-RPC error, as
// opposed to the call failing on the way, such as the connection to it
// breaking.
func answered(err error) bool {
	var wire *jsonrpc.Error
	return errors.As(err, &wire)
}

// Serve serves MCP to one Agent over stdin/stdout until the Agent
// disconnects or ctx is done.
func (g *Gateway) Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	return g.server.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: nopWriteCloser{stdout}})
}

// Close shuts every Upstream down at once, and waits for those still being
// stopped after failing to start.
func (g *Gateway) Close() {
	var wg sync.WaitGroup
	for _, s := range g.sessions {
		wg.Go(func() { _ = s.Close() })
	}
	wg.Wait()
	g.starting.Wait()
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
