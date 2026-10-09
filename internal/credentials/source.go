package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/marvell/sprut/internal/config"
)

// LoginNeededError is the error of an Upstream that has no Credentials it
// can use.
type LoginNeededError struct {
	// Reason is why the Credentials it had can't be used, "" if it had none.
	Reason string
}

func (e *LoginNeededError) Error() string {
	if e.Reason != "" {
		return e.Reason + ", so it needs a login"
	}
	return "needs a login"
}

// ReasonExpired is why an Upstream whose Credentials are dead (see
// Credentials.Dead) needs a Login.
const ReasonExpired = "the credentials expired and cannot be renewed"

// reasonRejected is why an Upstream that answers 401 to its Credentials
// needs a Login.
const reasonRejected = "the upstream rejected the credentials"

// reasonRefreshRejected is why an Upstream whose Authorization Server
// rejects the refresh token needs a Login.
const reasonRefreshRejected = "the authorization server rejected the credentials"

// ErrRefreshRejected is what a Refresh returns when the Authorization Server
// rejects the refresh token (invalid_grant).
var ErrRefreshRejected = errors.New("the authorization server rejected the refresh token")

// Refresh gets a new access token for c from their Authorization Server and
// returns the Credentials it makes. metadata is the URL of the Protected
// Resource Metadata that the Upstream named when it rejected c, if it did.
// It returns ErrRefreshRejected when the server rejects the refresh token,
// and a *LoginNeededError when c can't be used any more; any other error
// leaves them as they are.
type Refresh func(ctx context.Context, c *Credentials, metadata string) (*Credentials, error)

// renewBefore is how long before its access token expires that the
// Credentials are renewed, as oauth2.ReuseTokenSourceWithExpiry would.
const renewBefore = 5 * time.Minute

// Source is the Credentials of one Upstream as one process uses them. It
// reads them again whenever their file changes, so that a Login, a logout
// or another process's renewal shows, and renews them with its Refresh only
// once at a time, both in this process and, through the Store's lock,
// across processes (ADR 0003).
//
// Token on the nil Source returns no access token.
type Source struct {
	store    *Store
	upstream config.Upstream
	refresh  Refresh
	log      *slog.Logger

	mu    sync.Mutex
	creds *Credentials // nil without Credentials; replaced, never changed
	// version is the Credentials file that creds were last read from.
	version fileVersion
	// dead is why the Credentials can't be used any more, once they can't.
	dead error
	// rejected is the access token that the last renewal on a 401 got, which
	// is not renewed on a 401 again.
	rejected string
	renewing *renewal // in flight, if any
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

// Source returns the Source of u's Credentials, which renews them with
// refresh and logs to log.
func (s *Store) Source(u config.Upstream, refresh Refresh, log *slog.Logger) *Source {
	return &Source{store: s, upstream: u, refresh: refresh, log: log}
}

// Token returns the access token to send, renewed first if it expires
// within renewBefore, or "" without Credentials. A *LoginNeededError means
// that they can't be used.
func (s *Source) Token(ctx context.Context) (string, error) {
	if s == nil {
		return "", nil
	}
	if err := s.reload(); err != nil {
		return "", err
	}
	s.mu.Lock()
	dead, creds := s.dead, s.creds
	s.mu.Unlock()
	switch {
	case dead != nil:
		return "", dead
	case creds == nil:
		return "", nil
	case creds.Expiry.IsZero() || time.Until(creds.Expiry) > renewBefore:
		return creds.AccessToken, nil
	}
	return s.renew(ctx, creds.AccessToken, "", false)
}

// Rejected renews the Credentials whose access token the Upstream answered
// 401 to, at most once for each token, and returns the new access token.
// metadata is the URL of the Protected Resource Metadata that the 401 named,
// if any. A *LoginNeededError means that the Credentials can't be used.
func (s *Source) Rejected(ctx context.Context, token, metadata string) (string, error) {
	return s.renew(ctx, token, metadata, true)
}

// Unauthorized returns why the Upstream needs a Login when it answers 401
// past Token and Rejected: it has no Credentials, as last read, or rejects
// them.
func (s *Source) Unauthorized() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds == nil {
		return &LoginNeededError{}
	}
	return &LoginNeededError{Reason: reasonRejected}
}

// reload reads the Credentials again if their file is no longer the one
// they were read from: a Login, or another process, wrote it, or a logout
// deleted it. That is how serve picks up a new Login without a restart.
func (s *Source) reload() error {
	s.mu.Lock()
	last := s.version
	s.mu.Unlock()
	if !s.store.changed(s.upstream.Name, last) {
		return nil
	}
	creds, version, err := s.store.read(s.upstream)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.version != last {
		// Another request has read them meanwhile.
		version.close()
		return nil
	}
	s.version.close()
	s.creds, s.version, s.dead = creds, version, nil
	return nil
}

// renew renews the Credentials and returns their access token, unless the
// token is no longer stale, which means that another request has already
// renewed them. Every request that needs a renewal at the same time waits
// for the same one, which goes on even if they give up waiting: the
// Authorization Server may already have rotated the refresh token, and the
// new one must be kept. onRejection is set when the Upstream rejected stale
// with a 401. The renewal waits for the lock of the Credentials up to
// lockTimeout, and each request only as long as its ctx allows.
func (s *Source) renew(ctx context.Context, stale, metadata string, onRejection bool) (string, error) {
	s.mu.Lock()
	if token, err, settled := s.settledLocked(stale, onRejection); settled {
		s.mu.Unlock()
		return token, err
	}
	r := s.renewing
	if r == nil {
		r = &renewal{done: make(chan struct{})}
		s.renewing = r
		go func() {
			renewed, err := s.renewShared(context.WithoutCancel(ctx), stale, metadata)
			s.mu.Lock()
			defer s.mu.Unlock()
			switch {
			case err == nil:
				s.creds = renewed
				r.token = renewed.AccessToken
				if r.onRejection {
					s.rejected = renewed.AccessToken
				}
			case errors.As(err, new(*LoginNeededError)):
				s.dead = err
			}
			s.renewing = nil
			r.err = err
			close(r.done)
		}()
	}
	r.onRejection = r.onRejection || onRejection
	s.mu.Unlock()
	select {
	case <-r.done:
		return r.token, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// settledLocked returns the outcome of renewing the Credentials whose
// access token is stale, if it is known without a refresh. s.mu is held.
func (s *Source) settledLocked(stale string, onRejection bool) (token string, err error, settled bool) {
	switch {
	case s.dead != nil:
		return "", s.dead, true
	case s.creds == nil: // deleted since the request read them
		return "", &LoginNeededError{}, true
	case s.creds.AccessToken != stale:
		return s.creds.AccessToken, nil, true
	case onRejection && stale == s.rejected:
		return "", &LoginNeededError{Reason: reasonRejected}, true
	case s.creds.RefreshToken == "":
		switch {
		case s.creds.Dead():
			s.dead = &LoginNeededError{Reason: ReasonExpired}
			return "", s.dead, true
		case onRejection:
			return "", &LoginNeededError{Reason: reasonRejected}, true
		}
		return stale, nil, true // good until they expire
	}
	return "", nil, false
}

// renewShared renews the Credentials whose access token is stale through
// the Store, which renews them only once across processes, and returns
// them. A *LoginNeededError means that the Credentials can't be used any
// more; any other error leaves them as they are.
func (s *Source) renewShared(ctx context.Context, stale, metadata string) (*Credentials, error) {
	renewed, err := s.store.renew(ctx, s.upstream, stale, func(ctx context.Context, c *Credentials) (*Credentials, error) {
		return s.refresh(ctx, c, metadata)
	})
	switch {
	case renewed != nil && err != nil:
		// Still good for this process.
		s.log.Warn("renewed credentials not saved", "upstream", s.upstream.Name, "err", err)
		return renewed, nil
	case errors.Is(err, ErrRefreshRejected):
		return nil, &LoginNeededError{Reason: reasonRefreshRejected}
	case errors.As(err, new(*LoginNeededError)):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("renewing credentials: %w", err)
	case renewed == nil:
		return nil, &LoginNeededError{}
	}
	return renewed, nil
}
