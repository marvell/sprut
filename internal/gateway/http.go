package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/net/http/httpproxy"

	"github.com/marvell/sprut/internal/config"
)

// connectHTTP connects to u over Streamable HTTP and lists its tools. env
// is where the proxy comes from.
func connectHTTP(ctx context.Context, client *mcp.Client, u config.Upstream, env []string) (*ready, error) {
	t, err := newHTTPTransport(u, env)
	if err != nil {
		return nil, err
	}
	h := newStoredCredentials(u, env)
	if h != nil {
		t.OAuthHandler = h
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
	return list(ctx, session)
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
		rt = &headerTransport{scheme: endpoint.Scheme, host: endpoint.Host, headers: u.Headers, next: base}
	}
	return &mcp.StreamableClientTransport{
		Endpoint: u.URL,
		// No Timeout: the Gateway sets none on tool calls, and startup is
		// bounded by the ctx given to Connect.
		HTTPClient: &http.Client{Transport: rt},
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

// headerTransport adds headers to every request to scheme and host that it
// sends through next, so that a redirect elsewhere, or from HTTPS to plain
// HTTP on the same host, doesn't carry them, secrets included. A header the
// request already has, which the SDK sets for the protocol, is left as it is.
type headerTransport struct {
	scheme  string
	host    string
	headers map[string]string
	next    http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.scheme || req.URL.Host != t.host {
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
