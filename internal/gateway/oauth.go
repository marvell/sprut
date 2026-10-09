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
// it can use, with its remedy.
type needsLoginError struct {
	credentials.LoginNeededError
	hint string // the Upstream's loginHint
}

func (e *needsLoginError) Hint() string { return e.hint }

// loginHint is the remedy of an Upstream that needs a Login.
func loginHint(u config.Upstream) string {
	cmd := "run: sprut auth login "
	if u.ConfigPath != "" {
		cmd += "-c " + shellQuote(u.ConfigPath) + " "
	}
	return cmd + u.Name // a valid name needs no quoting
}

// shellQuote quotes s for a POSIX shell, unless it needs no quoting.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:@%+=,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

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
	source        *credentials.Source // nil when refused
	// refused is why the Upstream can't use OAuth, if it can't. It matters
	// only once the server asks for authorization.
	refused error
	oauth   http.RoundTripper // for requests to the Authorization Server and the metadata
	log     *slog.Logger

	mu  sync.Mutex
	err error // the last error a request or Authorize ended with
}

// openStore returns the Store that env points at, or nil if there is none
// (credentials.ErrNoHome). Every connection that one owner makes shares it,
// and the owner waits for its renewals before the process exits.
func openStore(env []string) *credentials.Store {
	store, _ := credentials.NewStore(config.LookupIn(env))
	return store
}

// newStoredCredentials returns the OAuth handler of u, with its Credentials
// in store, nil if there is none. oauth carries its requests to the
// Authorization Server. It returns nil for an Upstream that has an
// Authorization header, which never uses OAuth.
func newStoredCredentials(u config.Upstream, store *credentials.Store, oauth http.RoundTripper, log *slog.Logger) *storedCredentials {
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
	if store == nil {
		h.refused = credentials.ErrNoHome
		return h
	}
	h.source = store.Source(u, h.renewToken, log)
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
		return h.fail(&credentials.LoginNeededError{Reason: "the credentials lack a scope that the upstream needs"})
	}
	if resp.StatusCode != http.StatusUnauthorized {
		// Any other 403 is the server's answer to give; the request is
		// retried and fails as it is.
		return nil
	}
	if h.refused != nil {
		return h.fail(h.refused)
	}
	return h.fail(h.source.Unauthorized())
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
	token, err := h.source.Token(ctx)
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
	if token, err = h.source.Rejected(ctx, token, metadata); err != nil {
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

// renewToken is the credentials.Refresh of the Upstream. It first checks
// that the Upstream still names the Authorization Server that issued c.
func (h *storedCredentials) renewToken(ctx context.Context, c *credentials.Credentials, metadata string) (*credentials.Credentials, error) {
	issuer, err := h.issuer(ctx, c.Resource, metadata)
	if err != nil {
		return nil, fmt.Errorf("reading the protected resource metadata: %w", err)
	}
	if !sameIssuer(issuer, c.Issuer) {
		// The Credentials are not sent to a server they were not issued by.
		return nil, &credentials.LoginNeededError{Reason: "its authorization server has changed"}
	}

	cfg := &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: c.TokenURL, AuthStyle: c.AuthStyle}}
	cfg.ClientID, cfg.ClientSecret = h.client(c)
	client := oauthClient(withResource{resource: c.Resource, next: h.oauth})
	tok, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, client), &oauth2.Token{RefreshToken: c.RefreshToken}).Token()
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) && retrieve.ErrorCode == "invalid_grant" {
		return nil, credentials.ErrRefreshRejected
	}
	if err != nil {
		return nil, oauthError(err)
	}

	renewed := *c
	renewed.Token, renewed.Scopes = *tok, grantedScopes(tok, c.Scopes)
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

// needsLogin is the error of the Upstream needing a Login, for reason.
func (h *storedCredentials) needsLogin(reason string) error {
	return &needsLoginError{LoginNeededError: credentials.LoginNeededError{Reason: reason}, hint: loginHint(h.upstream)}
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

// maxErrorBytes bounds how much of an error response errorBodies reads.
const maxErrorBytes = 64 << 10

func (t errorBodies) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp.StatusCode < 400 {
		return resp, err
	}
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, maxErrorBytes)).Decode(&body)
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

// fail records err, if any, as the last error, and returns it, with the
// Upstream's loginHint if it is a *credentials.LoginNeededError.
func (h *storedCredentials) fail(err error) error {
	var e *credentials.LoginNeededError
	if errors.As(err, &e) {
		err = h.needsLogin(e.Reason)
	}
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
