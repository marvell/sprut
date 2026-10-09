package mcptest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HTTPUpstream is a fake HTTP Upstream: go-sdk's Streamable HTTP handler
// serving NewServer from an httptest server, which records the headers of
// every request it gets.
type HTTPUpstream struct {
	URL   string
	Close func() // stops the server, which then refuses connections

	mu      sync.Mutex
	headers []http.Header
}

// StartHTTP starts a fake HTTP Upstream supporting only protocol, or the
// SDK's versions if it is empty. It stops when the test ends.
func StartHTTP(t testing.TB, protocol string) *HTTPUpstream {
	t.Helper()
	server := NewServer(protocol)
	opts := &mcp.StreamableHTTPOptions{
		// Only a stateless handler serves the modern protocol; a legacy one
		// stays stateful, like most servers of that era.
		Stateless: protocol >= Modern,
		// Reached through a proxy, it gets requests for another host.
		DisableLocalhostProtection: true,
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, opts)
	u := &HTTPUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.headers = append(u.headers, r.Header.Clone())
		u.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	u.URL, u.Close = ts.URL, ts.Close
	return u
}

// Headers returns the headers of each request the Upstream got so far.
func (u *HTTPUpstream) Headers() []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.headers)
}

// StartSSEOnly starts a fake HTTP Upstream that speaks only the deprecated
// HTTP+SSE transport, and returns its URL.
func StartSSEOnly(t testing.TB) string {
	t.Helper()
	server := NewServer("")
	ts := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

// StartGETStream starts a fake Streamable HTTP Upstream that fails every
// POST but, as the transport allows of a server that needs no session id,
// answers a GET with an event stream, and returns its URL. The stream
// repeats chunk until the request ends; it stays silent if chunk is empty.
func StartGETStream(t testing.TB, chunk string) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "broken", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		for chunk != "" {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// StartHanging starts a fake HTTP Upstream that leaves its first request
// unanswered until the test ends, and returns its URL. Later requests, such
// as the client cancelling the first, are accepted at once, so that
// stopping the Upstream does not wait out the SDK's timeout.
func StartHanging(t testing.TB) string {
	t.Helper()
	release := make(chan struct{})
	var first sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hang := false
		first.Do(func() { hang = true })
		if hang {
			<-release
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(ts.Close)
	// The SDK leaves an abandoned request waiting for its answer, so Close
	// would wait for it forever.
	t.Cleanup(func() { close(release) })
	return ts.URL
}

// RefusingURL returns an HTTP URL at which nothing listens any more, so
// connecting to it is refused.
func RefusingURL() string {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	return gone.URL
}
