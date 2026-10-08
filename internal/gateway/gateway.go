// Package gateway connects to Upstreams and serves their tools to an Agent
// as Namespaced tools.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
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
	upstreams   *upstreams
	skipped     []skipped       // Upstreams that failed or timed out at startup
	tools       map[string]bool // Namespaced tools served; fixed once Start returns
	agentLogged atomic.Bool     // whether "agent connected" is logged
	log         *slog.Logger
}

// skipped is an Upstream left out at startup, and why.
type skipped struct {
	name string
	err  error
}

// Start launches every Upstream concurrently, collects their tools and
// returns a Gateway ready to serve them. env is the environment Upstream
// processes inherit. Each Upstream gets startupTimeout to connect and list
// its tools; one that fails or runs out of time is logged, left out and
// listed in the instructions the Agent gets.
func Start(ctx context.Context, configured []config.Upstream, env []string, startupTimeout time.Duration, version string, log *slog.Logger) *Gateway {
	impl := &mcp.Implementation{Name: "sprut", Version: version}
	g := &Gateway{
		upstreams: &upstreams{
			// The Gateway proxies no interactive input, so a modern Upstream's
			// request for it comes back to forward rather than being retried.
			client:  mcp.NewClient(impl, &mcp.ClientOptions{MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}}),
			env:     env,
			timeout: startupTimeout,
			log:     log,
		},
		tools: map[string]bool{},
		log:   log,
	}
	started, skips := g.upstreams.start(ctx, configured)
	g.skipped = skips

	// The server takes its instructions when it is made, so it is made once
	// the skipped Upstreams are known.
	g.server = mcp.NewServer(impl, &mcp.ServerOptions{
		// Advertise only tools; the tool list is fixed for the session.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		Instructions: instructions(skips),
	})
	g.server.AddReceivingMiddleware(g.answerUnknownTools, g.logAgentConnected)

	tools := 0
	for _, r := range started {
		added := g.addTools(r)
		log.Info("upstream ready", "upstream", r.name, "tools", added,
			"protocol", r.session.InitializeResult().ProtocolVersion, "duration", r.took)
		tools += added
	}
	log.Info("gateway started", "ready", len(started), "failed", len(skips), "tools", tools)
	return g
}

// Skipped reports how many Upstreams failed or timed out at startup.
func (g *Gateway) Skipped() int { return len(g.skipped) }

// instructions tells the Agent which Upstreams were skipped at startup and
// why, so that it can tell the user why their tools are missing. It is
// empty when none was.
func instructions(skipped []skipped) string {
	if len(skipped) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("These MCP servers failed to start, so their tools are missing. Tell the user why, and how to fix it where a fix is given:\n")
	for _, s := range skipped {
		fmt.Fprintf(&b, "- %s: %s\n", s.name, withHint(s.err))
	}
	return b.String()
}

// hinted is an error that carries a remedy the user can act on, such as a
// command to run.
type hinted interface {
	error
	Hint() string
}

// hint returns the remedy err carries, or "" if it carries none.
func hint(err error) string {
	var h hinted
	if errors.As(err, &h) {
		return h.Hint()
	}
	return ""
}

// withHint is err's message followed by its remedy, if it carries one.
func withHint(err error) string {
	if h := hint(err); h != "" {
		return fmt.Sprintf("%v (%s)", err, h)
	}
	return err.Error()
}

// addTools serves r's tools as Namespaced tools, skipping the ones addTool
// rejects, and returns how many it added.
func (g *Gateway) addTools(r *ready) int {
	added := 0
	for _, tool := range r.tools {
		if err := g.addTool(r.name, r.session, tool); err != nil {
			g.log.Warn("tool skipped", "upstream", r.name, "tool", r.name+separator+tool.Name, "err", err)
			continue
		}
		added++
	}
	return added
}

// ready is an Upstream that is connected and has listed its tools.
type ready struct {
	name    string
	session *mcp.ClientSession
	tools   []*mcp.Tool
	took    time.Duration // to start, which "upstream ready" reports, as it is logged once all are done
}

// upstreams starts the Upstreams and stops them again. Each gets timeout
// to connect and list its tools. env is the environment sprut was given: a
// stdio Upstream's process inherits it, and an HTTP Upstream's proxy comes
// from it.
type upstreams struct {
	client  *mcp.Client
	env     []string
	timeout time.Duration
	log     *slog.Logger

	sessions []*mcp.ClientSession // one per started Upstream
	starting sync.WaitGroup       // Upstream starts, including abandoned ones
}

// start starts every Upstream in configured concurrently, and returns those
// that started and those that failed or ran out of time, each in Config
// order. A skipped one is logged.
func (s *upstreams) start(ctx context.Context, configured []config.Upstream) ([]*ready, []skipped) {
	results := make([]struct {
		r   *ready
		err error
	}, len(configured))
	var wg sync.WaitGroup
	for i, u := range configured {
		wg.Go(func() {
			begin := time.Now()
			r, err := s.startOne(ctx, u)
			if err != nil {
				attrs := []any{"upstream", u.Name, "err", err}
				if h := hint(err); h != "" {
					attrs = append(attrs, "hint", h)
				}
				s.log.Warn("upstream failed; skipped", attrs...)
				results[i].err = err
				return
			}
			r.name, r.took = u.Name, time.Since(begin)
			results[i].r = r
		})
	}
	wg.Wait()

	// In Config order, so the log and the instructions don't depend on
	// which Upstream was quickest. (The SDK lists tools by name anyway.)
	var started []*ready
	var skips []skipped
	for i, res := range results {
		if res.err != nil {
			skips = append(skips, skipped{name: configured[i].Name, err: res.err})
			continue
		}
		started = append(started, res.r)
		s.sessions = append(s.sessions, res.r.session)
	}
	return started, skips
}

// startOne starts u, giving up on it once the timeout runs out.
func (s *upstreams) startOne(ctx context.Context, u config.Upstream) (*ready, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	// Connecting runs on its own, because a failed connect stops the
	// Upstream before returning, and a hung Upstream gets seconds of grace
	// to exit. Startup gives up at the deadline instead; close waits
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

// close stops every started Upstream at once, and waits for those still
// being stopped after failing to start.
func (s *upstreams) close() {
	var wg sync.WaitGroup
	for _, session := range s.sessions {
		wg.Go(func() { _ = session.Close() })
	}
	wg.Wait()
	s.starting.Wait()
}

// connect connects to u over its Transport and lists its tools.
func (s *upstreams) connect(ctx context.Context, u config.Upstream) (*ready, error) {
	switch u.Transport {
	case config.Stdio:
		return connectStdio(ctx, s.client, u, s.env, s.log)
	case config.HTTP:
		return connectHTTP(ctx, s.client, u, s.env)
	}
	return nil, fmt.Errorf("unknown transport %q", u.Transport)
}

// list lists the tools of the Upstream at the other end of session, and
// closes session if it can't.
func list(ctx context.Context, session *mcp.ClientSession) (*ready, error) {
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

// errorResult is an isError result saying err, and the remedy it carries,
// if any.
func errorResult(err error) *mcp.CallToolResult {
	res := &mcp.CallToolResult{}
	res.SetError(errors.New(withHint(err)))
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
func (g *Gateway) Close() { g.upstreams.close() }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
