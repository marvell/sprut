package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
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
type needsLoginError struct {
	upstream string
	reason   string // why the Credentials it had can't be used, if it had any
}

func (e *needsLoginError) Error() string {
	if e.reason != "" {
		return e.reason + ", so it needs a login"
	}
	return "needs a login"
}

func (e *needsLoginError) Hint() string { return "run: sprut auth login " + e.upstream }

// renewBefore is how long before its access token expires that the
// Credentials are renewed, as oauth2.ReuseTokenSourceWithExpiry would.
const renewBefore = 5 * time.Minute

// storedCredentials is the OAuth handler of an HTTP Upstream in serve. It
// sends the stored access token, if there is one, from the first request
// on, renews it when it is about to expire and once when the Upstream
// answers 401, and never starts a Login.
//
// The token goes on each request, and is renewed, in the Upstream's own
// transport (see wrap), not through the SDK's TokenSource and Authorize:
// the SDK gets a token without the request's ctx, so a hung Authorization
// Server would hold the call up past its cancellation, and it ends the
// whole connection when Authorize fails on a cancelled call.
type storedCredentials struct {
	name     string
	endpoint *url.URL // the Upstream's
	store    *credentials.Store
	// refused is why the Upstream can't use OAuth, if it can't, or why its
	// Credentials can't be read. It matters only once the server asks for
	// authorization.
	refused error
	oauth   http.RoundTripper // for requests to the Authorization Server and the metadata
	log     *slog.Logger

	mu    sync.Mutex
	creds *credentials.Credentials // nil without Credentials; replaced, never changed
	// dead is why the Credentials can't be used any more, once they can't.
	dead error
	// rejected is the access token that the last renewal on a 401 got, which
	// is not renewed on a 401 again.
	rejected string
	renewing *renewal // in flight, if any
	err      error    // the last error a request or Authorize ended with
}

// renewal is one renewal of the Credentials, which every request that needs
// it waits for.
type renewal struct {
	done  chan struct{}
	token string // the access token it got
	err   error
}

// newStoredCredentials returns the OAuth handler of u, loading its
// Credentials from the Store that env points at. oauth carries its
// requests to the Authorization Server. It returns nil for an Upstream that
// has an Authorization header, which never uses OAuth.
func newStoredCredentials(u config.Upstream, env []string, oauth http.RoundTripper, log *slog.Logger) *storedCredentials {
	if hasAuthorization(u) {
		return nil
	}
	h := &storedCredentials{name: u.Name, refused: CheckOAuthUpstream(u), oauth: oauth, log: log}
	h.endpoint, _ = url.Parse(u.URL) // parsed already by newHTTPTransport
	if h.refused != nil {
		return h
	}
	h.store, h.refused = credentials.NewStore(config.LookupIn(env))
	if h.refused == nil {
		h.creds, h.refused = h.store.Load(u.Name)
	}
	return h
}

// TokenSource returns nil: wrap's transport sends the token.
func (h *storedCredentials) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return nil, nil
}

// Authorize gets only the 401s that wrap's transport could not renew past.
func (h *storedCredentials) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	discard(resp)
	if resp.StatusCode != http.StatusUnauthorized {
		// A 403 is the server's answer to give; the request is retried and
		// fails as it is.
		return nil
	}
	if h.refused != nil {
		return h.fail(h.refused)
	}
	h.mu.Lock()
	has := h.creds != nil
	h.mu.Unlock()
	if !has {
		return h.fail(&needsLoginError{upstream: h.name})
	}
	return h.fail(&needsLoginError{upstream: h.name, reason: "the upstream rejected renewed credentials"})
}

// wrap returns the transport of requests to the Upstream, which sends them
// through next with the access token. It renews the token first if it is
// about to expire, and when the Upstream answers 401, then sends the
// request again.
func (h *storedCredentials) wrap(next http.RoundTripper) http.RoundTripper {
	return bearerTransport{h: h, next: next}
}

type bearerTransport struct {
	h    *storedCredentials
	next http.RoundTripper
}

func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h := t.h
	// Only to the Upstream itself, not wherever it redirects.
	if !toOrigin(req, h.endpoint.Scheme, h.endpoint.Host) {
		return t.next.RoundTrip(req)
	}
	ctx := req.Context()
	token, err := h.token(ctx)
	if err != nil {
		return nil, h.fail(err)
	}
	resp, err := t.send(req, token)
	canResend := req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
	if err != nil || resp.StatusCode != http.StatusUnauthorized || token == "" || !canResend {
		return resp, err
	}
	metadata := resourceMetadataURL(resp)
	discard(resp)
	if token, err = h.renew(ctx, token, metadata, true); err != nil {
		return nil, h.fail(err)
	}
	if req.GetBody != nil {
		req = req.Clone(ctx)
		if req.Body, err = req.GetBody(); err != nil {
			return nil, err
		}
	}
	return t.send(req, token)
}

// send sends req through next with token, if there is one.
func (t bearerTransport) send(req *http.Request, token string) (*http.Response, error) {
	if token != "" {
		// A RoundTripper must not modify the request it is given.
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return t.next.RoundTrip(req)
}

// resourceMetadataURL returns the URL of the Protected Resource Metadata
// that a 401 names, if it names one.
func resourceMetadataURL(resp *http.Response) string {
	challenges, _ := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	for _, c := range challenges {
		if m := c.Params["resource_metadata"]; m != "" {
			return m
		}
	}
	return ""
}

// discard closes resp's body, drained, but only so far, so that the
// connection can be reused.
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// token returns the access token to send, renewed first if it expires
// within renewBefore, or "" without Credentials.
func (h *storedCredentials) token(ctx context.Context) (string, error) {
	h.mu.Lock()
	dead, creds := h.dead, h.creds
	h.mu.Unlock()
	switch {
	case dead != nil:
		return "", dead
	case creds == nil:
		return "", nil
	case creds.Expiry.IsZero() || time.Until(creds.Expiry) > renewBefore:
		return creds.AccessToken, nil
	}
	return h.renew(ctx, creds.AccessToken, "", false)
}

// renew renews the Credentials and returns their access token, unless the
// token is no longer stale, which means that another request has already
// renewed them. Every request that needs a renewal at the same time waits
// for the same one, which goes on even if they give up waiting: the
// Authorization Server may already have rotated the refresh token, and the
// new one must be kept. onRejection is set when the Upstream rejected stale
// with a 401, and metadata is then the URL of its Protected Resource
// Metadata, if it named one.
func (h *storedCredentials) renew(ctx context.Context, stale, metadata string, onRejection bool) (string, error) {
	h.mu.Lock()
	if token, err, settled := h.settledLocked(stale, onRejection); settled {
		h.mu.Unlock()
		return token, err
	}
	r := h.renewing
	if r == nil {
		r = &renewal{done: make(chan struct{})}
		h.renewing = r
		c := *h.creds
		go func() {
			renewed, err := h.refresh(context.WithoutCancel(ctx), &c, metadata)
			h.mu.Lock()
			defer h.mu.Unlock()
			switch {
			case err == nil:
				h.creds = renewed
				r.token = renewed.AccessToken
				if onRejection {
					h.rejected = renewed.AccessToken
				}
			case errors.As(err, new(*needsLoginError)):
				h.dead = err
			}
			h.renewing = nil
			r.err = err
			close(r.done)
		}()
	}
	h.mu.Unlock()
	select {
	case <-r.done:
		return r.token, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// settledLocked returns the outcome of renewing the Credentials whose
// access token is stale, if it is known without a refresh. h.mu is held.
func (h *storedCredentials) settledLocked(stale string, onRejection bool) (token string, err error, settled bool) {
	switch {
	case h.dead != nil:
		return "", h.dead, true
	case h.creds.AccessToken != stale:
		return h.creds.AccessToken, nil, true
	case onRejection && stale == h.rejected:
		return "", &needsLoginError{upstream: h.name, reason: "the upstream rejected renewed credentials"}, true
	case h.creds.RefreshToken == "":
		if onRejection || time.Now().After(h.creds.Expiry) {
			h.dead = &needsLoginError{upstream: h.name, reason: "the credentials expired and cannot be renewed"}
			return "", h.dead, true
		}
		return stale, nil, true // good until they expire
	}
	return "", nil, false
}

// refresh gets a new access token for c from its Authorization Server,
// once it has checked that the Upstream still names that server, and
// stores the Credentials it makes. A needsLoginError means that c can't be
// used any more; any other error leaves c as it is.
func (h *storedCredentials) refresh(ctx context.Context, c *credentials.Credentials, metadata string) (*credentials.Credentials, error) {
	issuer, err := h.issuer(ctx, c.Resource, metadata)
	if err != nil {
		return nil, fmt.Errorf("renewing credentials: reading the protected resource metadata: %w", err)
	}
	if !sameIssuer(issuer, c.Issuer) {
		// The Credentials are not sent to a server they were not issued by.
		return nil, &needsLoginError{upstream: h.name, reason: "its authorization server has changed"}
	}

	cfg := &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: c.TokenURL, AuthStyle: c.AuthStyle},
	}
	client := &http.Client{Transport: withResource{resource: c.Resource, next: h.oauth}, Timeout: oauthTimeout}
	tok, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, client), &oauth2.Token{RefreshToken: c.RefreshToken}).Token()
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant" {
		return nil, &needsLoginError{upstream: h.name, reason: "the authorization server rejected the credentials"}
	}
	if err != nil {
		return nil, fmt.Errorf("renewing credentials: %w", oauthError(err))
	}

	cfg.Scopes = c.Scopes
	renewed := newCredentials(cfg, tok)
	renewed.Resource, renewed.Issuer = c.Resource, c.Issuer
	if err := h.store.Save(h.name, renewed); err != nil {
		// Still good for this process.
		h.log.Warn("renewed credentials not saved", "upstream", h.name, "err", err)
	}
	h.log.Debug("credentials renewed", "upstream", h.name, "expires", renewed.Expiry)
	return renewed, nil
}

// issuer returns the issuer of the Authorization Server that the Upstream
// whose canonical URI is resource names in its Protected Resource Metadata,
// looked up where the SDK looks for it at a Login: at metadata, if set, then
// at the well-known URIs (RFC 9728). An Upstream without the metadata is its
// own Authorization Server.
func (h *storedCredentials) issuer(ctx context.Context, resource, metadata string) (string, error) {
	ru, err := url.Parse(resource)
	if err != nil {
		return "", err
	}
	root := (&url.URL{Scheme: ru.Scheme, Host: ru.Host}).String()
	type candidate struct{ link, resource string }
	candidates := []candidate{{wellKnown(ru, ru.Path), resource}, {wellKnown(ru, ""), root}}
	if metadata != "" {
		candidates = append([]candidate{{metadata, resource}}, candidates...)
	}

	client := &http.Client{Transport: h.oauth, Timeout: oauthTimeout}
	for _, c := range candidates {
		if link, err := url.Parse(c.link); err != nil || !isSecure(link) {
			continue
		}
		meta, err := getResourceMetadata(ctx, client, c.link)
		if err != nil {
			return "", err
		}
		if meta == nil || meta.Resource != c.resource {
			continue
		}
		if len(meta.AuthorizationServers) == 0 {
			return "", errors.New("it names no authorization server")
		}
		return meta.AuthorizationServers[0], nil
	}
	return root, nil
}

// wellKnown returns where the Protected Resource Metadata of a resource
// with path is, on u's origin (RFC 9728, section 3.1).
func wellKnown(u *url.URL, path string) string {
	path = strings.TrimSuffix("/.well-known/oauth-protected-resource/"+strings.TrimLeft(path, "/"), "/")
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: path}).String()
}

// getResourceMetadata returns the Protected Resource Metadata at link, or
// nil if there is none there. A failure to tell, such as a network error or
// a 5xx, is an error.
func getResourceMetadata(ctx context.Context, client *http.Client, link string) (*oauthex.ProtectedResourceMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, nil
	}
	var meta oauthex.ProtectedResourceMetadata
	if json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(&meta) != nil {
		return nil, nil
	}
	return &meta, nil
}

// sameIssuer reports whether a and b name the same issuer, but for a
// trailing slash, which servers are careless about.
func sameIssuer(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

// fail records err, if any, as the last error, and returns it.
func (h *storedCredentials) fail(err error) error {
	if err != nil {
		h.mu.Lock()
		h.err = err
		h.mu.Unlock()
	}
	return err
}

// Err returns the last error that a request or Authorize ended with, which
// the SDK reports only wrapped in its own.
func (h *storedCredentials) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// withResource adds the RFC 8707 resource parameter to the token requests
// it sends through next, which oauth2's refresh leaves out.
type withResource struct {
	resource string
	next     http.RoundTripper
}

func (t withResource) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.Body == nil {
		return t.next.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	form.Set("resource", t.resource)
	encoded := form.Encode()
	req = req.Clone(req.Context())
	req.Body = io.NopCloser(strings.NewReader(encoded))
	req.ContentLength = int64(len(encoded))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(encoded)), nil }
	return t.next.RoundTrip(req)
}
