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

func (e *needsLoginError) Hint() string { return loginHint(e.upstream) }

// loginHint is the remedy of an Upstream that needs a Login.
func loginHint(upstream string) string { return "run: sprut auth login " + upstream }

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
	upstream config.Upstream
	endpoint *url.URL // the Upstream's
	// the client that the Config registers, if any, which the Credentials
	// don't hold
	preregistered *oauthex.ClientCredentials
	store         *credentials.Store
	// refused is why the Upstream can't use OAuth, if it can't, or why its
	// Credentials can't be read. It matters only once the server asks for
	// authorization.
	refused error
	oauth   http.RoundTripper // for requests to the Authorization Server and the metadata
	log     *slog.Logger

	mu    sync.Mutex
	creds *credentials.Credentials // nil without Credentials; replaced, never changed
	// version is the Credentials file that creds were last read from.
	version credentials.Version
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
	done chan struct{}
	// onRejection is set once any request that waits for it got a 401.
	onRejection bool
	token       string // the access token it got
	err         error
}

// newStoredCredentials returns the OAuth handler of u, loading its
// Credentials from the Store that env points at. oauth carries its
// requests to the Authorization Server. It returns nil for an Upstream that
// has an Authorization header, which never uses OAuth.
func newStoredCredentials(u config.Upstream, env []string, oauth http.RoundTripper, log *slog.Logger) *storedCredentials {
	if hasAuthorization(u) {
		return nil
	}
	h := &storedCredentials{
		upstream: u, preregistered: preregisteredClient(u), refused: CheckOAuthUpstream(u), oauth: oauth, log: log,
	}
	h.endpoint, _ = url.Parse(u.URL) // parsed already by newHTTPTransport
	if h.refused != nil {
		return h
	}
	h.store, h.refused = credentials.NewStore(config.LookupIn(env))
	if h.refused == nil {
		h.creds, h.version, h.refused = h.store.Read(u)
	}
	return h
}

// TokenSource returns nil: wrap's transport sends the token.
func (h *storedCredentials) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return nil, nil
}

// Authorize gets only the 401s that wrap's transport could not renew past,
// and the 403s.
func (h *storedCredentials) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	discard(resp)
	if resp.StatusCode == http.StatusForbidden && insufficientScope(resp) {
		return h.fail(h.needsLogin("the credentials lack a scope that the upstream needs"))
	}
	if resp.StatusCode != http.StatusUnauthorized {
		// Any other 403 is the server's answer to give; the request is
		// retried and fails as it is.
		return nil
	}
	if h.refused != nil {
		return h.fail(h.refused)
	}
	h.mu.Lock()
	has := h.creds != nil
	h.mu.Unlock()
	if !has {
		return h.fail(h.needsLogin(""))
	}
	return h.fail(h.needsLogin(rejected))
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
	if !sameOrigin(req.URL, h.endpoint) {
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
	return challengeParam(resp, "resource_metadata")
}

// insufficientScope reports whether resp's challenge says that the access
// token lacks a scope (RFC 6750, section 3.1).
func insufficientScope(resp *http.Response) bool {
	return challengeParam(resp, "error") == "insufficient_scope"
}

// challengeParam returns the parameter name of the first challenge of resp
// that has it, or "".
func challengeParam(resp *http.Response, name string) string {
	challenges, _ := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	for _, c := range challenges {
		if v := c.Params[name]; v != "" {
			return v
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
	if err := h.reload(); err != nil {
		return "", err
	}
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

// reload reads the Credentials again if their file is no longer the one
// they were read from: a Login, or another process, wrote it, or a logout
// deleted it. That is how serve picks up a new Login without a restart.
func (h *storedCredentials) reload() error {
	if h.store == nil {
		return nil
	}
	h.mu.Lock()
	last := h.version
	h.mu.Unlock()
	if !h.store.Changed(h.upstream.Name, last) {
		return nil
	}
	creds, version, err := h.store.Read(h.upstream)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.version != last {
		// Another request has read them meanwhile.
		version.Close()
		return nil
	}
	h.version.Close()
	h.creds, h.version, h.dead = creds, version, nil
	return nil
}

// renew renews the Credentials and returns their access token, unless the
// token is no longer stale, which means that another request has already
// renewed them. Every request that needs a renewal at the same time waits
// for the same one, which goes on even if they give up waiting: the
// Authorization Server may already have rotated the refresh token, and the
// new one must be kept. onRejection is set when the Upstream rejected stale
// with a 401, and metadata is then the URL of its Protected Resource
// Metadata, if it named one. The renewal waits for the lock of the
// Credentials up to the Store's 30s, and each request only as long as its
// ctx allows.
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
		go func() {
			renewed, err := h.refresh(context.WithoutCancel(ctx), stale, metadata)
			h.mu.Lock()
			defer h.mu.Unlock()
			switch {
			case err == nil:
				h.creds = renewed
				r.token = renewed.AccessToken
				if r.onRejection {
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
	r.onRejection = r.onRejection || onRejection
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
	case h.creds == nil: // deleted since the request read them
		return "", h.needsLogin(""), true
	case h.creds.AccessToken != stale:
		return h.creds.AccessToken, nil, true
	case onRejection && stale == h.rejected:
		return "", h.needsLogin(rejected), true
	case h.creds.RefreshToken == "":
		switch {
		case h.creds.Dead():
			h.dead = h.needsLogin(expired)
			return "", h.dead, true
		case onRejection:
			return "", h.needsLogin(rejected), true
		}
		return stale, nil, true // good until they expire
	}
	return "", nil, false
}

// refresh renews the Credentials whose access token is stale, under their
// lock, so that only one process renews them at a time, and returns them.
// It reads them again first: if another process has already renewed them,
// those are returned; if they are gone, nothing is written. Otherwise it
// gets a new access token from their Authorization Server, once it has
// checked that the Upstream still names that server, and stores the
// Credentials it makes. A needsLoginError means that the Credentials can't
// be used any more; any other error leaves them as they are.
func (h *storedCredentials) refresh(ctx context.Context, stale, metadata string) (*credentials.Credentials, error) {
	lock, err := h.store.Lock(ctx, h.upstream.Name)
	if err != nil {
		return nil, fmt.Errorf("renewing credentials: %w", err)
	}
	defer lock.Unlock()
	c, err := h.store.Load(h.upstream)
	switch {
	case err != nil:
		return nil, fmt.Errorf("renewing credentials: %w", err)
	case c == nil:
		return nil, h.needsLogin("")
	case c.AccessToken != stale:
		return c, nil // renewed by another process
	}

	issuer, err := h.issuer(ctx, c.Resource, metadata)
	if err != nil {
		return nil, fmt.Errorf("renewing credentials: reading the protected resource metadata: %w", err)
	}
	if !sameIssuer(issuer, c.Issuer) {
		// The Credentials are not sent to a server they were not issued by.
		return nil, h.needsLogin("its authorization server has changed")
	}

	cfg := &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: c.TokenURL, AuthStyle: c.AuthStyle}}
	cfg.ClientID, cfg.ClientSecret = h.client(c)
	client := oauthClient(withResource{resource: c.Resource, next: h.oauth})
	tok, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, client), &oauth2.Token{RefreshToken: c.RefreshToken}).Token()
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant" {
		// Unless a process that did not wait for the lock has rotated the
		// refresh token meanwhile.
		again, err := h.store.Load(h.upstream)
		if err != nil {
			// Unknown, so they are kept.
			return nil, fmt.Errorf("renewing credentials: %w", err)
		}
		if again != nil && again.RefreshToken != c.RefreshToken {
			return again, nil
		}
		return nil, h.needsLogin("the authorization server rejected the credentials")
	}
	if err != nil {
		return nil, fmt.Errorf("renewing credentials: %w", oauthError(err))
	}

	renewed := *c
	renewed.Token, renewed.Scopes = *tok, grantedScopes(tok, c.Scopes)
	if err := lock.Save(&renewed); err != nil {
		// Still good for this process.
		h.log.Warn("renewed credentials not saved", "upstream", h.upstream.Name, "err", err)
	}
	h.log.Debug("credentials renewed", "upstream", h.upstream.Name, "expires", renewed.Expiry)
	return &renewed, nil
}

// client returns the id and secret of the client that renews c: the one
// the Config registers, if any, else the one registered at the Login.
func (h *storedCredentials) client(c *credentials.Credentials) (id, secret string) {
	p := h.preregistered
	if p == nil {
		return c.ClientID, c.ClientSecret
	}
	if p.ClientSecretAuth != nil {
		secret = p.ClientSecretAuth.ClientSecret
	}
	return p.ClientID, secret
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

	client := oauthClient(h.oauth)
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

// expired is why an Upstream whose Credentials are Dead needs a Login.
const expired = "the credentials expired and cannot be renewed"

// rejected is why an Upstream that answers 401 to its Credentials needs a
// Login.
const rejected = "the upstream rejected the credentials"

// needsLogin is the error of the Upstream needing a Login, for reason.
func (h *storedCredentials) needsLogin(reason string) error {
	return &needsLoginError{upstream: h.upstream.Name, reason: reason}
}

// oauthClient is the HTTP client of requests to an Authorization Server
// and of metadata lookups, through rt.
func oauthClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: errorBodies{next: rt}, Timeout: oauthTimeout}
}

// errorBodies reduces the body of every error response it gets from next
// to the OAuth error code in it, if any: the SDK and oauth2 quote the bodies
// of error responses in their errors, and a server may echo a secret there
// or in its error_description.
type errorBodies struct{ next http.RoundTripper }

func (t errorBodies) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp.StatusCode < 400 {
		return resp, err
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(&body)
	_ = resp.Body.Close()
	reduced := "{}"
	if body.Error != "" {
		data, _ := json.Marshal(map[string]string{"error": body.Error})
		reduced = string(data)
	}
	resp.Header = resp.Header.Clone()
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Del("Content-Length")
	resp.Body = io.NopCloser(strings.NewReader(reduced))
	resp.ContentLength = int64(len(reduced))
	return resp, nil
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
