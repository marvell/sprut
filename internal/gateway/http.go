package gateway

import (
	"context"
	"mime"
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/net/http/httpproxy"

	"github.com/marvell/sprut/internal/config"
)

// newHTTPTransport returns the transport to u over Streamable HTTP. Its
// requests to u's host carry u's Config headers, and go through the proxy
// that env names, if any.
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
		rt = &headerTransport{host: endpoint.Host, headers: u.Headers, next: base}
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

// sseOnly reports whether the server at t's endpoint answers a GET with an
// event stream, as an HTTP+SSE server does to open a session. A Streamable
// HTTP server never does: without a session it refuses the GET.
func sseOnly(ctx context.Context, t *mcp.StreamableClientTransport) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	// The stream stays open; its headers are all this needs.
	_ = resp.Body.Close()
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return resp.StatusCode == http.StatusOK && media == "text/event-stream"
}

// headerTransport adds headers to every request to host that it sends
// through next, so that a redirect elsewhere doesn't carry them, secrets
// included. A header the request already has, which the SDK sets for the
// protocol, is left as it is.
type headerTransport struct {
	host    string
	headers map[string]string
	next    http.RoundTripper
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != t.host {
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
