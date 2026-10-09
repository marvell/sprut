package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/net/http/httpproxy"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

// connectHTTP connects to u over Streamable HTTP and lists its tools. env
// is where the proxy comes from, and store holds the Credentials.
func connectHTTP(ctx context.Context, client *mcp.Client, u config.Upstream, env []string, store *credentials.Store, log *slog.Logger) (*ready, error) {
	session, err := dialHTTP(ctx, client, u, env, store, log)
	if err != nil {
		return nil, err
	}
	return list(ctx, session)
}

// dialHTTP connects to u over Streamable HTTP, with its stored Credentials
// if it is an OAuth Upstream.
func dialHTTP(ctx context.Context, client *mcp.Client, u config.Upstream, env []string, store *credentials.Store, log *slog.Logger) (*mcp.ClientSession, error) {
	t, err := newHTTPTransport(u, env)
	if err != nil {
		return nil, err
	}
	h := newStoredCredentials(u, store, t.HTTPClient.Transport, log)
	if h != nil {
		t.OAuthHandler = h
		t.HTTPClient.Transport = h.wrap(t.HTTPClient.Transport)
	}
	session, err := client.Connect(ctx, t, nil)
	if err != nil {
		if h != nil && h.Err() != nil {
			return nil, h.Err()
		}
		// The SDK never falls back to the deprecated HTTP+SSE transport, and
		// an SSE-only server fails here with a bare HTTP status, so say why.
		if sseOnly(ctx, t) {
			err = fmt.Errorf("server speaks only the deprecated HTTP+SSE transport, which is not supported: %w", err)
		}
		return nil, err
	}
	return session, nil
}

// newHTTPTransport returns the transport to u over Streamable HTTP. Its
// requests to u's scheme and host carry u's Config headers, and go through
// the proxy that env names, if any.
func newHTTPTransport(u config.Upstream, env []string) (*mcp.StreamableClientTransport, error) {
	endpoint, err := url.Parse(u.URL)
	if err != nil {
		return nil, err
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	// From the env sprut was given, not the real one, which
	// http.ProxyFromEnvironment would read.
	proxy := proxyConfig(config.LookupIn(env)).ProxyFunc()
	base.Proxy = func(req *http.Request) (*url.URL, error) { return proxy(req.URL) }
	var rt http.RoundTripper = base
	if len(u.Headers) > 0 {
		rt = &headerTransport{endpoint: endpoint, headers: u.Headers, next: base}
	}
	// No Timeout: the Gateway sets none on tool calls, and startup is
	// bounded by the ctx given to Connect.
	client := &http.Client{Transport: rt}
	if CheckOAuthUpstream(u) == nil {
		client.CheckRedirect = sameOriginRedirects(endpoint)
	}
	return &mcp.StreamableClientTransport{
		Endpoint:   u.URL,
		HTTPClient: client,
		// The tool list is fixed at startup, so there is nothing for a
		// stream of server-initiated messages to deliver.
		DisableStandaloneSSE: true,
	}, nil
}

// proxyConfig reads the proxy variables as net/http does: the upper-case
// name first.
func proxyConfig(lookupEnv config.LookupEnv) *httpproxy.Config {
	get := func(names ...string) string {
		for _, name := range names {
			if v, ok := lookupEnv(name); ok && v != "" {
				return v
			}
		}
		return ""
	}
	return &httpproxy.Config{
		HTTPProxy:  get("HTTP_PROXY", "http_proxy"),
		HTTPSProxy: get("HTTPS_PROXY", "https_proxy"),
		NoProxy:    get("NO_PROXY", "no_proxy"),
	}
}

// How long sseOnly waits for the endpoint event, and the most of the GET
// stream that it reads. An HTTP+SSE server sends that event as soon as the
// stream opens.
const (
	probeTimeout  = 2 * time.Second
	probeMaxBytes = 64 << 10
)

// sseOnly reports whether the server at t's endpoint answers a GET with the
// endpoint event that opens an HTTP+SSE session. An event stream alone
// proves nothing: a Streamable HTTP server that needs no session id may
// serve one too.
func sseOnly(ctx context.Context, t *mcp.StreamableClientTransport) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	client := *t.HTTPClient
	// The SDK caps each event's size, but not the stream's: a run of empty
	// events would go on until the timeout.
	client.Transport = limitTransport{next: client.Transport, n: probeMaxBytes}
	conn, err := (&mcp.SSEClientTransport{Endpoint: t.Endpoint, HTTPClient: &client}).Connect(ctx)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// limitTransport ends each response body it gets from next after n bytes.
type limitTransport struct {
	next http.RoundTripper
	n    int64
}

func (t limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = limitedBody{Reader: io.LimitReader(resp.Body, t.n), Closer: resp.Body}
	return resp, nil
}

type limitedBody struct {
	io.Reader
	io.Closer
}

// headerTransport adds headers to every request to the origin of endpoint
// that it sends through next, so that a redirect elsewhere, or from HTTPS
// to plain HTTP on the same host, doesn't carry them, secrets included. A
// header the request already has, which the SDK sets for the protocol, is
// left as it is.
type headerTransport struct {
	endpoint *url.URL
	headers  map[string]string
	next     http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !sameOrigin(req.URL, t.endpoint) {
		return t.next.RoundTrip(req)
	}
	// A RoundTripper must not modify the request it is given.
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	return t.next.RoundTrip(req)
}

// sameOrigin reports whether a and b have the same origin: scheme, host
// and port, the default port of the scheme included. It is the one origin
// that an Upstream's secrets may go to.
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// port returns the port of u, or the default port of its scheme.
func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// maxRedirects is how many redirects in a row a request follows, as
// net/http's default.
const maxRedirects = 10

// sameOriginRedirects is the CheckRedirect of an OAuth Upstream's client: it
// follows a redirect only within the origin of endpoint, so that a request
// to the Upstream fails in place of going, body and all, anywhere else, a
// subdomain or plain HTTP on the same host included. (bearerTransport adds
// the access token only within that origin anyway.)
func sameOriginRedirects(endpoint *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(req.URL, endpoint) {
			return fmt.Errorf("refused a redirect to another origin, %s://%s", req.URL.Scheme, req.URL.Host)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
}
