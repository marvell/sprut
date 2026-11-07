// Package gateway connects to Upstreams and serves their tools to an Agent
// as Namespaced tools.
package gateway

import (
	"context"
	"encoding/json"
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

	"github.com/marvell/sprut/internal/audit"
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
	skipped     []skippedUpstream // Upstreams that failed or timed out at startup
	catalog     catalog           // the Namespaced tools served; fixed once Start returns
	codeMode    bool              // whether the execute tool is served too
	agentLogged atomic.Bool       // whether "agent connected" is logged
	scripts     atomic.Int64      // the Scripts run so far
	version     string
	audit       *audit.Log // nil without one
	log         *slog.Logger
}

// skippedUpstream is an Upstream left out at startup, and why.
type skippedUpstream struct {
	name string
	err  error
}

// Start launches every Upstream concurrently, collects their tools and
// returns a Gateway ready to serve them. env is the environment Upstream
// processes inherit. Each Upstream gets startupTimeout to connect and list
// its tools; one that fails or runs out of time is logged, left out and
// listed in the instructions the Agent gets. auths holds the Credentials
// of every OAuth Upstream; Close waits for their renewals in progress.
// With a codeMode, the Gateway also serves the execute tool of Code mode.
// With an auditLog, it writes the Agent's session, its calls and its
// Scripts there.
func Start(ctx context.Context, configured []config.Upstream, env []string, auths *Auths, startupTimeout time.Duration, codeMode *CodeMode, auditLog *audit.Log, version string, log *slog.Logger) *Gateway {
	g := &Gateway{
		upstreams: newUpstreams(env, auths, startupTimeout, version, log),
		catalog:   catalog{},
		version:   version,
		audit:     auditLog,
		log:       log,
	}
	// In Config order, so the log and the instructions don't depend on
	// which Upstream was quickest. (The SDK lists tools by name anyway.)
	var running []*ready
	var skips []skippedUpstream
	for i, res := range g.upstreams.start(ctx, configured) {
		if res.err != nil {
			skips = append(skips, skippedUpstream{name: configured[i].Name, err: res.err})
			continue
		}
		running = append(running, res.r)
	}
	g.skipped = skips

	// The server takes its instructions when it is made, so it is made once
	// the skipped Upstreams are known.
	g.server = mcp.NewServer(implementation(version), &mcp.ServerOptions{
		// Advertise only tools; the tool list is fixed for the session.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		Instructions: instructions(skips),
	})
	g.server.AddReceivingMiddleware(g.answerUnknownTools, g.agentConnected)

	tools := 0
	for _, r := range running {
		added := g.addTools(r)
		log.Info("upstream ready", "upstream", r.name, "tools", added,
			"protocol", r.session.InitializeResult().ProtocolVersion, "duration", r.took)
		tools += added
	}
	if codeMode != nil {
		g.addExecute(codeMode)
	}
	log.Info("gateway started", "ready", len(running), "failed", len(skips), "tools", tools)
	return g
}

// newClient is sprut's MCP client to Upstreams.
func newClient(version string) *mcp.Client {
	// The Gateway proxies no interactive input, so a modern Upstream's
	// request for it comes back to forward rather than being retried.
	return mcp.NewClient(implementation(version), &mcp.ClientOptions{MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}})
}

// implementation is how sprut names itself, to the Agent and to Upstreams.
func implementation(version string) *mcp.Implementation {
	return &mcp.Implementation{Name: "sprut", Version: version}
}

// SkippedCount reports how many Upstreams failed or timed out at startup.
func (g *Gateway) SkippedCount() int { return len(g.skipped) }

// instructions tells the Agent which Upstreams were skipped at startup and
// why, so that it can tell the user why their tools are missing. It is
// empty when none was.
func instructions(skipped []skippedUpstream) string {
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
// from it. auths holds the Credentials of every OAuth Upstream.
type upstreams struct {
	client  *mcp.Client
	env     []string
	auths   *Auths
	timeout time.Duration
	log     *slog.Logger

	sessions []*mcp.ClientSession // one per started Upstream
	starting sync.WaitGroup       // Upstream starts, including abandoned ones
}

// startResult is how one Upstream fared at startup: ready, or the error it
// failed with.
type startResult struct {
	r   *ready
	err error
}

// start starts every Upstream in configured concurrently, and returns how
// each fared, in Config order. A failed one is logged. The ready ones are
// stopped by close.
func (s *upstreams) start(ctx context.Context, configured []config.Upstream) []startResult {
	results := make([]startResult, len(configured))
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
	for _, res := range results {
		if res.r != nil {
			s.sessions = append(s.sessions, res.r.session)
		}
	}
	return results
}

// newUpstreams returns the starter of Upstreams for a sprut of version,
// whose environment is env.
func newUpstreams(env []string, auths *Auths, timeout time.Duration, version string, log *slog.Logger) *upstreams {
	return &upstreams{client: newClient(version), env: env, auths: auths, timeout: timeout, log: log}
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

// close stops every started Upstream at once, waits for those still being
// stopped after failing to start, and then for the renewals of Credentials
// in progress, which closing a session may itself start.
func (s *upstreams) close() {
	var wg sync.WaitGroup
	for _, session := range s.sessions {
		wg.Go(func() { _ = session.Close() })
	}
	wg.Wait()
	s.starting.Wait()
	s.auths.Wait()
}

// connect connects to u over its Transport and lists its tools.
func (s *upstreams) connect(ctx context.Context, u config.Upstream) (*ready, error) {
	switch u.Transport {
	case config.Stdio:
		return connectStdio(ctx, s.client, u, s.env, s.log)
	case config.HTTP:
		return connectHTTP(ctx, s.client, s.auths.For(u), s.log)
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
	t := &namespacedTool{
		name:      namespaced.Name,
		upstream:  upstream,
		session:   session,
		original:  original,
		readOnly:  tool.Annotations != nil && tool.Annotations.ReadOnlyHint,
		log:       log,
		scriptLog: log.With("via", executeTool),
		audit:     g.audit,
	}
	g.server.AddTool(&namespaced, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return t.call(ctx, req.Params.Arguments, nil)
	})
	g.catalog[namespaced.Name] = t
	t.log.Debug("tool added")
	return nil
}

// catalog holds the Namespaced tools the Gateway serves, by name.
type catalog map[string]*namespacedTool

// namespacedTool is an Upstream's tool, as the Gateway serves it.
type namespacedTool struct {
	name      string // as the Agent sees it
	upstream  string
	session   *mcp.ClientSession
	original  string       // the tool's name at the Upstream
	readOnly  bool         // whether the Upstream marks it read-only
	log       *slog.Logger // for direct calls
	scriptLog *slog.Logger // for calls from a Script
	audit     *audit.Log
}

// answerUnknownTools answers a call to a tool the Gateway doesn't serve with
// an isError result, where the SDK would answer with a protocol error.
func (g *Gateway) answerUnknownTools(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if call, ok := req.(*mcp.CallToolRequest); ok && !g.serves(call.Params.Name) {
			return errorResult(fmt.Errorf("unknown tool %q", call.Params.Name)), nil
		}
		return next(ctx, method, req)
	}
}

// serves reports whether the Gateway serves the tool named name.
func (g *Gateway) serves(name string) bool {
	return g.catalog[name] != nil || (g.codeMode && name == executeTool)
}

// agentConnected logs, once, the protocol version the Agent speaks, and
// starts the Audit log of its session, as soon as the SDK has settled it:
// by the initialize handshake for a legacy Agent, or from the first
// request of a modern one, even one that fails, before handling it.
func (g *Gateway) agentConnected(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		g.noteAgent(req, nil)
		res, err := next(ctx, method, req)
		g.noteAgent(req, res)
		return res, err
	}
}

// noteAgent logs the Agent's protocol version and starts the Audit log,
// unless they are done or req and its result res, if any, don't settle
// them yet.
func (g *Gateway) noteAgent(req mcp.Request, res mcp.Result) {
	if g.agentLogged.Load() {
		return
	}
	ss, _ := req.GetSession().(*mcp.ServerSession)
	var params *mcp.InitializeParams
	if ss != nil {
		params = ss.InitializeParams()
	}
	var protocol string
	if init, ok := res.(*mcp.InitializeResult); ok {
		protocol = init.ProtocolVersion // negotiated, unlike the one asked for
	} else if _, initializing := req.(*mcp.InitializeRequest); params != nil && !initializing {
		protocol = params.ProtocolVersion
	}
	if protocol == "" || !g.agentLogged.CompareAndSwap(false, true) {
		return
	}
	g.log.Info("agent connected", "protocol", protocol)
	s := audit.Session{Sprut: g.version, Protocol: protocol, CodeMode: g.codeMode}
	if params != nil && params.ClientInfo != nil {
		s.Agent, s.AgentVersion = params.ClientInfo.Name, params.ClientInfo.Version
	}
	g.audit.Start(s)
}

// call calls t at its Upstream with args, for the Script run, or directly
// if it is nil, and logs the call, to t's log and to the Audit log. A
// call the Upstream doesn't answer, such as one to an Upstream that is
// gone, ends in an isError result naming it, and so does one where it asks
// for interactive input.
func (t *namespacedTool) call(ctx context.Context, args json.RawMessage, run *scriptRun) (*mcp.CallToolResult, error) {
	begin := time.Now()
	res, err := t.session.CallTool(ctx, &mcp.CallToolParams{Name: t.original, Arguments: args})
	took, callErr := time.Since(begin), err
	switch {
	case res == nil:
	case res.NeedsInput():
		res = errorResult(fmt.Errorf("upstream %q asked for input mid-call, but interactive input is not supported by the gateway", t.upstream))
	default:
		// serverInfo identifies the responder of each hop; drop the
		// Upstream's so the Gateway's own is reported to the Agent.
		delete(res.Meta, mcp.MetaKeyServerInfo)
	}
	if err != nil && ctx.Err() == nil && !answered(err) {
		res, err = errorResult(fmt.Errorf("call to upstream %q failed: %w", t.upstream, err)), nil
	}
	log, script := t.log, 0
	if run != nil {
		log, script = t.scriptLog, run.n
	}
	logCall(log, "tool called", took, res, callErr)
	if t.audit != nil {
		outcome, why := outcome(ctx, res, err)
		t.audit.Call(audit.Call{Tool: t.name, Upstream: t.upstream, Script: script, Took: took,
			Outcome: outcome, Err: why, ArgsBytes: len(args), ResultBytes: size(res)})
	}
	return res, err
}

// outcome is how a call that ended in res or err, under ctx, went for the
// Audit log, and why, if it failed.
func outcome(ctx context.Context, res *mcp.CallToolResult, err error) (audit.Outcome, string) {
	switch {
	case err != nil && ctx.Err() != nil:
		return audit.Cancelled, ""
	case err != nil:
		return audit.Error, err.Error()
	case res.IsError:
		return audit.Error, text(res)
	}
	return audit.OK, ""
}

// size is the size of res's JSON, or 0 for no res. It leaves out
// resultType, which the SDK stamps on each hop to a modern peer, so that a
// result has the same size whichever era its Upstream and Agent are of.
// The field is unexported, so it is subtracted from the encoding.
func size(res *mcp.CallToolResult) int {
	if res == nil {
		return 0
	}
	data, err := json.Marshal(res)
	if err != nil {
		return 0
	}
	n := len(data)
	if m := resultTypeField.FindAllIndex(data, -1); m != nil {
		last := m[len(m)-1] // the SDK's own, which follows the result's fields
		n -= last[1] - last[0]
	}
	return n
}

// resultTypeField is resultType in a result's JSON, with the comma before
// it: the SDK writes it after content, which is always there.
var resultTypeField = regexp.MustCompile(`,"resultType":"[a-z_]*"`)

// logCall logs at DEBUG a call that took took and ended in res, or in err
// on the way there.
func logCall(log *slog.Logger, msg string, took time.Duration, res *mcp.CallToolResult, err error) {
	attrs := []any{"duration", took}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	log.Debug(msg, append(attrs, "isError", res != nil && res.IsError)...)
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
// breaking or its request never being sent.
func answered(err error) bool {
	var wire *jsonrpc.Error
	return errors.As(err, &wire) && !isRejected(wire)
}

// isRejected reports whether wire is the SDK's mark of a request that its
// transport never got an answer to (jsonrpc2.ErrRejected, which the SDK
// doesn't export), such as one whose HTTP request failed.
func isRejected(wire *jsonrpc.Error) bool {
	return wire.Code == -32005 && wire.Message == "rejected by transport"
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
