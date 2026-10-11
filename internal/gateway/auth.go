package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

// Auths are the Logins and Credentials of every Upstream of one process,
// whose Credentials are in one Store. Every Auth of a process shares it, so
// that its renewals are waited for before the process exits.
type Auths struct {
	env      []string
	store    *credentials.Store // nil when it can't be located
	storeErr error              // why store is nil
	remedy   func(config.Upstream) string
}

// OpenAuths returns the Auths whose Store env points at. remedy is what the
// user runs to log in to an Upstream: the cli's command, which only the cli
// knows. env is also where a Login's proxy and browser come from.
//
// A Store that can't be located is not an error here: it is one only for
// what needs it, and an Upstream that is not an OAuth Upstream never does.
func OpenAuths(env []string, remedy func(config.Upstream) string) *Auths {
	store, err := credentials.NewStore(config.LookupIn(env))
	return &Auths{env: env, store: store, storeErr: err, remedy: remedy}
}

// Located returns why the Store can't be located, or nil if it can.
func (a *Auths) Located() error {
	return a.storeErr
}

// Forget deletes whatever Credentials are stored under the Upstream name,
// whether or not the Config still has an OAuth Upstream by that name, and
// reports whether there were any.
func (a *Auths) Forget(ctx context.Context, name string) (bool, error) {
	if a.storeErr != nil {
		return false, a.storeErr
	}
	return a.store.Delete(ctx, name)
}

// Wait waits for the renewals of Credentials in progress, as
// credentials.Store.Wait does.
func (a *Auths) Wait() {
	a.store.Wait()
}

// For returns the Auth of u.
func (a *Auths) For(u config.Upstream) *Auth {
	return &Auth{auths: a, upstream: u, refused: checkOAuth(u)}
}

// Auth is the Login and Credentials of one Upstream: whether it is an
// OAuth Upstream at all, its Credentials and their status, and, in serve,
// the access token its requests carry.
type Auth struct {
	auths    *Auths
	upstream config.Upstream
	// refused is why the Upstream is not an OAuth Upstream, nil if it is.
	refused error
}

// store returns the Store of the Credentials of an OAuth Upstream, or why
// there is none: the Upstream is not an OAuth Upstream, or the Store can't
// be located.
func (a *Auth) store() (*credentials.Store, error) {
	if a.refused != nil {
		return nil, a.refused
	}
	return a.auths.store, a.auths.storeErr
}

// Logout deletes the Credentials of an OAuth Upstream, under their lock,
// and reports whether there were any. The lock file stays (ADR 0003).
func (a *Auth) Logout(ctx context.Context) (bool, error) {
	store, err := a.store()
	if err != nil {
		return false, err
	}
	return store.Delete(ctx, a.upstream.Name)
}

// Remedy is what the user runs to log in to the Upstream.
func (a *Auth) Remedy() string {
	return a.auths.remedy(a.upstream)
}

// Status returns the status of the Upstream's Credentials, as the Store
// holds them. A disabled Upstream is judged as if enabled.
func (a *Auth) Status() AuthStatus {
	if a.refused != nil {
		return AuthStatus{State: AuthNotOAuth}
	}
	store, err := a.store()
	if err != nil {
		return AuthStatus{State: AuthUnreadable, Err: err}
	}
	c, err := store.Load(a.upstream)
	switch {
	case err != nil:
		return AuthStatus{State: AuthUnreadable, Err: err}
	case c == nil:
		// No remedy: an Upstream that never asks for authorization has
		// none either, and needs no Login.
		return AuthStatus{State: AuthNoCredentials}
	case c.Dead():
		return AuthStatus{State: AuthDead, Reason: credentials.ReasonExpired, Remedy: a.Remedy()}
	}
	return AuthStatus{
		State:     AuthLoggedIn,
		Expiry:    c.Expiry,
		Expired:   !c.Expiry.IsZero() && time.Now().After(c.Expiry),
		Renewable: c.RefreshToken != "",
	}
}

// transport returns the transport to the HTTP Upstream, as newHTTPTransport
// does. An OAuth Upstream's is not redirected to another origin, where its
// access token would follow.
func (a *Auth) transport() (*mcp.StreamableClientTransport, error) {
	t, err := newHTTPTransport(a.upstream, a.auths.env)
	if err != nil {
		return nil, err
	}
	if a.refused == nil {
		endpoint, _ := url.Parse(a.upstream.URL) // parsed by newHTTPTransport
		t.HTTPClient.CheckRedirect = sameOriginRedirects(endpoint)
	}
	return t, nil
}

// checkOAuth reports why u is not an OAuth Upstream, or nil if it is: an
// HTTP Upstream without an Authorization header, over HTTPS or over plain
// HTTP on a loopback address. Whether it actually uses OAuth is up to the
// Upstream, which asks for it by answering 401.
func checkOAuth(u config.Upstream) error {
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
