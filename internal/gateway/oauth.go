package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"sync"

	"golang.org/x/oauth2"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

// CheckOAuthUpstream reports why u is not an OAuth Upstream, or nil if it
// is: an HTTP Upstream without an Authorization header, over HTTPS or over
// plain HTTP on a loopback address. Whether it actually uses OAuth is up
// to the Upstream, which asks for it by answering 401.
func CheckOAuthUpstream(u config.Upstream) error {
	if u.Transport != config.HTTP {
		return fmt.Errorf("uses %s, and OAuth is only for HTTP upstreams", u.Transport)
	}
	if hasAuthorization(u) {
		return errors.New("sends an Authorization header from the config, so it does not use OAuth")
	}
	endpoint, err := url.Parse(u.URL)
	if err != nil {
		return err
	}
	if isSecure(endpoint) {
		return nil
	}
	// Its tokens would cross the network in the clear.
	return errors.New("OAuth requires HTTPS")
}

// hasAuthorization reports whether u's Config headers include an
// Authorization header, which rules OAuth out.
func hasAuthorization(u config.Upstream) bool {
	for k := range u.Headers {
		if http.CanonicalHeaderKey(k) == "Authorization" {
			return true
		}
	}
	return false
}

// isSecure reports whether u is HTTPS, or plain HTTP to a loopback address,
// which never crosses the network.
func isSecure(u *url.URL) bool {
	return u.Scheme == "https" || u.Scheme == "http" && isLoopback(u.Hostname())
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// needsLoginError is the error of an OAuth Upstream that has no Credentials
// it can use.
type needsLoginError struct{ upstream string }

func (e *needsLoginError) Error() string { return "needs a login" }
func (e *needsLoginError) Hint() string  { return "run: sprut auth login " + e.upstream }

// storedCredentials is the OAuth handler of an HTTP Upstream in serve. It
// sends the stored access token, if there is one, from the first request on,
// and never starts a Login: a 401 means the Upstream needs one.
type storedCredentials struct {
	tokens oauth2.TokenSource // nil without Credentials
	// refused is why the Upstream can't use OAuth, if it can't, or why its
	// Credentials can't be read. It matters only once the server asks for
	// authorization.
	refused error
	name    string

	mu  sync.Mutex
	err error // the last error Authorize returned
}

// newStoredCredentials returns the OAuth handler of u, loading its
// Credentials from the Store that env points at. It returns nil for an
// Upstream that has an Authorization header, which never uses OAuth.
func newStoredCredentials(u config.Upstream, env []string) *storedCredentials {
	if hasAuthorization(u) {
		return nil
	}
	h := &storedCredentials{name: u.Name, refused: CheckOAuthUpstream(u)}
	if h.refused != nil {
		return h
	}
	store, err := credentials.NewStore(config.LookupIn(env))
	if err != nil {
		h.refused = err
		return h
	}
	c, err := store.Load(u.Name)
	switch {
	case err != nil:
		h.refused = err
	case c != nil:
		h.tokens = oauth2.StaticTokenSource(&c.Token)
	}
	return h
}

func (h *storedCredentials) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return h.tokens, nil
}

func (h *storedCredentials) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	// Drained, but only so far, so that the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		// A 403 is the server's answer to give; the request is retried and
		// fails as it is.
		return nil
	}
	err := h.refused
	if err == nil {
		err = &needsLoginError{upstream: h.name}
	}
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
	return err
}

// Err returns the last error Authorize returned, which the SDK reports
// only wrapped in its own.
func (h *storedCredentials) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}
