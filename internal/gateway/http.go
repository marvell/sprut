package gateway

import (
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/config"
)

// httpTransport connects to u over Streamable HTTP, sending its Config
// headers with every request to u's host.
func httpTransport(u config.Upstream) (*mcp.StreamableClientTransport, error) {
	endpoint, err := url.Parse(u.URL)
	if err != nil {
		return nil, err
	}
	headers := http.DefaultTransport
	if len(u.Headers) > 0 {
		headers = &headerTransport{host: endpoint.Host, headers: u.Headers, next: http.DefaultTransport}
	}
	return &mcp.StreamableClientTransport{
		Endpoint: u.URL,
		// No Timeout: the Gateway sets none on tool calls, and startup is
		// bounded by the ctx given to Connect.
		HTTPClient: &http.Client{Transport: headers},
		// The tool list is fixed at startup, so there is nothing for a
		// stream of server-initiated messages to deliver.
		DisableStandaloneSSE: true,
	}, nil
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
