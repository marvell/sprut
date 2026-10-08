package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

const (
	// oauthTimeout bounds each request to an Authorization Server.
	oauthTimeout = 30 * time.Second
	// callbackTimeout bounds the wait for the user to finish in the browser.
	callbackTimeout = 5 * time.Minute
)

// Login runs the Login for u and stores the Credentials it gets. It
// registers a client with Dynamic Client Registration, prints the
// Authorization Server's issuer, the scopes and the authorization URL to
// prompt, opens the URL if browser is set, and waits a while for the
// Authorization Server to redirect the user's browser to a callback on a
// loopback port. env is where the proxy and the Credentials' directory come
// from.
func Login(ctx context.Context, u config.Upstream, env []string, browser bool, prompt io.Writer, version string, log *slog.Logger) error {
	if err := CheckOAuthUpstream(u); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callbackTimeout)
	defer cancel()
	store, err := credentials.NewStore(config.LookupIn(env))
	if err != nil {
		return err
	}

	// Bound before anything else, as the redirect URI names its port.
	callbacks, err := listenForCallback()
	if err != nil {
		return err
	}
	defer callbacks.Close()

	t, err := newHTTPTransport(u, env)
	if err != nil {
		return err
	}
	// The SDK's discovery adds its SSRF checks only to a bare
	// *http.Transport; a wrapped one, like this, is used as it is.
	issuer := &issuerRecorder{next: t.HTTPClient.Transport}
	var (
		resource string
		asIssuer string
		creds    *credentials.Credentials
	)
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:      "sprut",
				ApplicationType: "native",
				RedirectURIs:    []string{callbacks.URL},
				GrantTypes:      []string{"authorization_code", "refresh_token"},
				ResponseTypes:   []string{"code"},
			},
		},
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			authURL, err := url.Parse(args.URL)
			if err != nil {
				return nil, err
			}
			q := authURL.Query()
			resource = q.Get("resource")
			asIssuer = issuer.Issuer()
			if asIssuer == "" {
				// Without metadata, the SDK takes the Authorization Server's
				// URL, under which the endpoints are, as its issuer.
				asIssuer = strings.TrimSuffix(authURL.Scheme+"://"+authURL.Host+authURL.Path, "/authorize")
			}
			scopes := q.Get("scope")
			if scopes == "" {
				scopes = "(none)"
			}
			_, _ = fmt.Fprintf(prompt, "Logging in to %s.\n  authorization server: %s\n  scopes: %s\n\n", u.Name, asIssuer, scopes)
			lead := "Open this URL in your browser:"
			if browser {
				lead = "Opening this URL in your browser; if it does not open, open it yourself:"
			}
			_, _ = fmt.Fprintf(prompt, "%s\n\n  %s\n\n", lead, args.URL)
			if browser {
				if err := openBrowser(args.URL, env); err != nil {
					_, _ = fmt.Fprintf(prompt, "Could not open a browser: %v\n\n", err)
				}
			}
			callbacks.Expect(q.Get("state"), asIssuer)
			select {
			case cb := <-callbacks.Results:
				return cb.result, cb.err
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return nil, fmt.Errorf("no callback from the browser within %v; run the login again", callbackTimeout)
				}
				return nil, ctx.Err()
			}
		},
		RequestRefreshToken:   true,
		AcceptUnadvertisedIss: true,
		Client:                &http.Client{Transport: issuer, Timeout: oauthTimeout},
		// Called with the final client and token once the code is exchanged:
		// all that the Credentials need.
		NewTokenSource: func(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			creds = newCredentials(cfg, tok)
			return cfg.TokenSource(ctx, tok), nil
		},
	})
	if err != nil {
		return err
	}
	once := &authorizeOnce{AuthorizationCodeHandler: h}
	t.OAuthHandler = once

	// Connecting is what makes the Upstream ask for authorization.
	session, err := newClient(version).Connect(ctx, t, nil)
	if err != nil {
		// The SDK's own wrapping of it quotes its requests.
		if loginErr := once.Err(); loginErr != nil {
			err = loginErr
		}
		return oauthError(err)
	}
	_ = session.Close()
	if creds == nil {
		return errors.New("it did not ask for authorization, so it needs no login")
	}
	creds.Resource, creds.Issuer = resource, asIssuer
	if err := store.Save(u.Name, creds); err != nil {
		return err
	}
	log.Info("login complete", "upstream", u.Name, "scopes", strings.Join(creds.Scopes, " "))
	return nil
}

// openBrowser opens link in the user's browser. The link is one argument
// to the opener, never a shell's to parse, and must be HTTPS, or HTTP on a
// loopback address.
func openBrowser(link string, env []string) error {
	parsed, err := url.Parse(link)
	if err != nil {
		return err
	}
	if !isSecure(parsed) {
		return errors.New("the authorization URL is not HTTPS")
	}
	var opener string
	switch runtime.GOOS {
	case "darwin":
		opener = "open"
	case "linux":
		opener = "xdg-open"
	default:
		return fmt.Errorf("not supported on %s", runtime.GOOS)
	}
	cmd := exec.Command(opener, link)
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reaped, whatever it says: the printed URL works either way.
	go func() { _ = cmd.Wait() }()
	return nil
}

// newCredentials are the Credentials of tok, issued to the client of cfg.
func newCredentials(cfg *oauth2.Config, tok *oauth2.Token) *credentials.Credentials {
	scopes := cfg.Scopes
	// The server says which scopes it granted only if they differ from
	// those requested (RFC 6749, section 5.1).
	if granted, ok := tok.Extra("scope").(string); ok && granted != "" {
		scopes = strings.Fields(granted)
	}
	return &credentials.Credentials{
		Token:        *tok,
		Scopes:       scopes,
		TokenURL:     cfg.Endpoint.TokenURL,
		AuthStyle:    cfg.Endpoint.AuthStyle,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
	}
}

// authorizeOnce runs the Login at the first request that asks for
// authorization, and answers every later one with its outcome: when the
// modern handshake fails, Connect tries the legacy one, which asks again.
type authorizeOnce struct {
	*auth.AuthorizationCodeHandler

	mu  sync.Mutex
	ran bool
	err error
}

func (a *authorizeOnce) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ran {
		_ = resp.Body.Close()
		return a.err
	}
	a.ran = true
	a.err = a.AuthorizationCodeHandler.Authorize(ctx, req, resp)
	return a.err
}

// Err returns the error of the Login, if it ran and failed.
func (a *authorizeOnce) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

// callback is what the Authorization Server sent to the callback.
type callback struct {
	result *auth.AuthorizationResult
	err    error
}

// callbackServer is the loopback listener that the Authorization Server
// redirects the user's browser to at the end of a Login. It accepts exactly
// one callback, the first GET /callback with the state it expects, and then
// stops listening.
type callbackServer struct {
	URL     string        // the redirect URI
	Results chan callback // the one callback accepted

	srv *http.Server

	mu     sync.Mutex
	state  string // expected; empty until the authorization URL is made
	issuer string // the Authorization Server's
	done   bool
}

func listenForCallback() (*callbackServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for the callback: %w", err)
	}
	c := &callbackServer{
		URL:     "http://" + ln.Addr().String() + "/callback",
		Results: make(chan callback, 1),
	}
	c.srv = &http.Server{Handler: c, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = c.srv.Serve(ln) }()
	return c, nil
}

// Expect sets the state that the callback must carry, and the issuer
// that its iss, if any, must name.
func (c *callbackServer) Expect(state, issuer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state, c.issuer = state, issuer
}

func (c *callbackServer) Close() { _ = c.srv.Close() }

func (c *callbackServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/callback" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		http.Error(w, "sprut: this login is already complete", http.StatusConflict)
		return
	}
	// Checked before anything else in it is read: a request without the
	// state is not this Login's, but a stale tab's or a forgery.
	if c.state == "" || q.Get("state") != c.state {
		http.Error(w, "sprut: this is not the callback of the login in progress", http.StatusBadRequest)
		return
	}
	c.done = true
	// Nothing more is accepted; the response still goes out.
	go func() { _ = c.srv.Shutdown(context.Background()) }()

	iss := q.Get("iss")
	cb := callback{result: &auth.AuthorizationResult{Code: q.Get("code"), State: c.state, Iss: iss}}
	if iss != "" && iss != c.issuer {
		// RFC 9207: compared even when the server doesn't say it sends iss.
		// Another server's response, so nothing in it is shown, not even
		// an error code.
		cb = callback{err: errors.New("the authorization response came from another issuer, so it was rejected")}
	} else if code := q.Get("error"); code != "" {
		// Only the error code: its description is the server's to word.
		cb = callback{err: fmt.Errorf("authorization server answered %q", code)}
	}
	c.Results <- cb
	if cb.err != nil {
		http.Error(w, "sprut: the login failed; see the terminal.", http.StatusBadRequest)
		return
	}
	_, _ = io.WriteString(w, "sprut: done. You can close this tab.\n")
}

// oauthError reduces an error from an Authorization Server to its OAuth
// error code and HTTP status: the messages of the SDK and of oauth2 quote
// the server's description and response body, which may echo a secret.
func oauthError(err error) error {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		if retrieve.ErrorCode != "" {
			return fmt.Errorf("authorization server answered %q (HTTP %d)", retrieve.ErrorCode, retrieve.Response.StatusCode)
		}
		return fmt.Errorf("authorization server answered HTTP %d", retrieve.Response.StatusCode)
	}
	var registration *oauthex.ClientRegistrationError
	if errors.As(err, &registration) {
		return fmt.Errorf("client registration failed: authorization server answered %q", registration.ErrorCode)
	}
	return err
}

// issuerRecorder sends requests through next and keeps the issuer named
// by the Authorization Server metadata that the SDK fetched, which it
// doesn't pass on.
type issuerRecorder struct {
	next http.RoundTripper

	mu     sync.Mutex
	issuer string
}

func (r *issuerRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK || !isAuthServerMetadata(req.URL.Path) {
		return resp, err
	}
	// Bounded as the SDK bounds it.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	var meta oauthex.AuthServerMeta
	if json.Unmarshal(body, &meta) == nil && meta.Issuer != "" {
		r.mu.Lock()
		r.issuer = meta.Issuer
		r.mu.Unlock()
	}
	return resp, nil
}

func (r *issuerRecorder) Issuer() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.issuer
}

// isAuthServerMetadata reports whether path is one where Authorization
// Server metadata is discovered (RFC 8414 and OpenID Connect Discovery).
func isAuthServerMetadata(path string) bool {
	return strings.HasPrefix(path, "/.well-known/oauth-authorization-server") ||
		strings.Contains(path, "/.well-known/openid-configuration")
}
