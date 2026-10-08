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
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

// oauthTimeout bounds each request to an Authorization Server.
const oauthTimeout = 30 * time.Second

// Login runs the Login for u and stores the Credentials it gets. It
// registers a client with Dynamic Client Registration, prints the
// authorization URL to prompt, and waits for the Authorization Server to
// redirect the user's browser to a callback on a loopback port. env is where
// the proxy and the Credentials' directory come from.
func Login(ctx context.Context, u config.Upstream, env []string, prompt io.Writer, version string, log *slog.Logger) error {
	if err := OAuthUpstream(u); err != nil {
		return err
	}
	store, err := credentials.NewStore(config.LookupIn(env))
	if err != nil {
		return err
	}

	// Bound before anything else, as the redirect URI names its port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listening for the callback: %w", err)
	}
	callbacks := make(chan callback, 1)
	srv := &http.Server{Handler: callbackHandler(callbacks), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	redirectURL := "http://" + ln.Addr().String() + "/callback"

	t, err := newHTTPTransport(u, env)
	if err != nil {
		return err
	}
	// The SDK's discovery adds its SSRF checks only to a bare
	// *http.Transport; a wrapped one, like this, is used as it is.
	issuer := &issuerRecorder{next: t.HTTPClient.Transport}
	var (
		resource string
		got      *credentials.Credentials
	)
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:      "sprut",
				ApplicationType: "native",
				RedirectURIs:    []string{redirectURL},
				GrantTypes:      []string{"authorization_code", "refresh_token"},
				ResponseTypes:   []string{"code"},
			},
		},
		AuthorizationCodeFetcher: func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			if authURL, err := url.Parse(args.URL); err == nil {
				resource = authURL.Query().Get("resource")
			}
			_, _ = fmt.Fprintf(prompt, "To log in to %s, open this URL in your browser:\n\n  %s\n\n", u.Name, args.URL)
			select {
			case cb := <-callbacks:
				return cb.result, cb.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		RequestRefreshToken:   true,
		AcceptUnadvertisedIss: true,
		Client:                &http.Client{Transport: issuer, Timeout: oauthTimeout},
		// Called with the final client and token once the code is exchanged:
		// all that the Credentials need.
		NewTokenSource: func(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
			got = newCredentials(cfg, tok)
			return cfg.TokenSource(ctx, tok), nil
		},
	})
	if err != nil {
		return err
	}
	t.OAuthHandler = h

	// Connecting is what makes the server ask for authorization.
	session, err := newClient(version).Connect(ctx, t, nil)
	if err != nil {
		return oauthError(err)
	}
	_ = session.Close()
	if got == nil {
		return errors.New("the server did not ask for authorization, so it needs no login")
	}
	got.Resource, got.Issuer = resource, issuer.get()
	if err := store.Save(u.Name, got); err != nil {
		return err
	}
	log.Info("login complete", "upstream", u.Name, "scopes", strings.Join(got.Scopes, " "))
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

// callback is what the Authorization Server sent to the callback.
type callback struct {
	result *auth.AuthorizationResult
	err    error
}

// callbackHandler serves the callback of a Login, passing the first
// request to it on to callbacks.
func callbackHandler(callbacks chan<- callback) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cb := callback{result: &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}}
		if code := q.Get("error"); code != "" {
			// Only the error code: its description is the server's to word.
			cb = callback{err: fmt.Errorf("authorization server answered %q", code)}
		}
		select {
		case callbacks <- cb:
			_, _ = io.WriteString(w, "sprut: done. You can close this tab.\n")
		default:
			http.Error(w, "sprut: this login is already complete", http.StatusConflict)
		}
	})
	return mux
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

func (r *issuerRecorder) get() string {
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
