// Package credentials stores the Credentials of each OAuth Upstream in a
// file of its own, written only under a lock on a sibling file (ADR 0003).
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"golang.org/x/oauth2"

	"github.com/marvell/sprut/internal/config"
)

// Credentials are what a Login leaves behind for one Upstream: its tokens,
// what is needed to renew them, and the identity they were issued for.
type Credentials struct {
	oauth2.Token
	Scopes []string `json:"scopes,omitempty"` // granted

	TokenURL  string           `json:"token_url"`
	AuthStyle oauth2.AuthStyle `json:"auth_style"`
	// The client that Dynamic Client Registration issued, if any.
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`

	// Identity: the canonical URI of the Upstream and the issuer of the
	// Authorization Server, and the Config they were issued for.
	Resource string  `json:"resource"`
	Issuer   string  `json:"issuer"`
	Config   Binding `json:"config"`
}

// Dead reports whether c can't be used any more without a new Login: the
// access token has expired, and there is no refresh token to renew it with.
func (c *Credentials) Dead() bool {
	return c.RefreshToken == "" && !c.Expiry.IsZero() && time.Now().After(c.Expiry)
}

// Binding is what an Upstream's Config said when its Credentials were
// issued, and must still say for them to be used. oauth.clientSecret is
// left out, so that rotating it keeps them.
type Binding struct {
	URL      string   `json:"url"`
	ClientID string   `json:"client_id,omitempty"` // oauth.clientId
	Scopes   []string `json:"scopes"`              // oauth.scopes; null when the Config has none
}

// BindingOf returns the Binding of u as its Config is now.
func BindingOf(u config.Upstream) Binding {
	b := Binding{URL: u.URL}
	if u.OAuth != nil {
		b.ClientID, b.Scopes = u.OAuth.ClientID, slices.Clone(u.OAuth.Scopes)
	}
	return b
}

// equal reports whether b and o are the same, but for the order of the
// scopes.
func (b Binding) equal(o Binding) bool {
	return b.URL == o.URL && b.ClientID == o.ClientID && (b.Scopes == nil) == (o.Scopes == nil) &&
		slices.Equal(slices.Sorted(slices.Values(b.Scopes)), slices.Sorted(slices.Values(o.Scopes)))
}

// Store is the directory holding the Credentials of every Upstream.
type Store struct {
	dir string

	mu       sync.Mutex
	idle     sync.Cond // signalled when renewing drops to 0
	renewing int       // Renew calls in progress
}

// NewStore returns the Store under $XDG_STATE_HOME/sprut/credentials, or
// ~/.local/state/sprut/credentials.
func NewStore(lookupEnv config.LookupEnv) (*Store, error) {
	if dir, ok := config.XDGDir(lookupEnv, "XDG_STATE_HOME", filepath.Join(".local", "state")); ok {
		s := &Store{dir: filepath.Join(dir, "sprut", "credentials")}
		s.idle.L = &s.mu
		return s, nil
	}
	return nil, ErrNoHome
}

// ErrNoHome is why NewStore can't return a Store.
var ErrNoHome = errors.New("cannot locate the credentials: HOME is not set")

// lockTimeout bounds the wait for another process to release the lock.
const lockTimeout = 30 * time.Second

// Version is one write of an Upstream's Credentials file, as last read: every
// write makes a new file, so a Version that is no longer the file means that
// the Credentials were written or deleted since. The zero Version is no
// file.
type Version struct {
	// Kept open, so that the system can't give its inode to a later write,
	// which would then pass for this one.
	f  *os.File
	fi os.FileInfo
}

// Close releases v.
func (v Version) Close() {
	if v.f != nil {
		_ = v.f.Close()
	}
}

// Load returns the Credentials of u, or nil if it has none, or none issued
// for u as its Config is now.
func (s *Store) Load(u config.Upstream) (*Credentials, error) {
	c, v, err := s.Read(u)
	v.Close()
	return c, err
}

// Read is Load, and also returns the Version of the file it read, which the
// caller closes.
func (s *Store) Read(u config.Upstream) (*Credentials, Version, error) {
	f, err := os.Open(s.path(u.Name, ".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, Version{}, nil
	}
	if err != nil {
		return nil, Version{}, fmt.Errorf("reading credentials: %w", err)
	}
	var c Credentials
	if err := json.NewDecoder(f).Decode(&c); err != nil {
		_ = f.Close()
		return nil, Version{}, fmt.Errorf("reading credentials %s: %w", f.Name(), err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Version{}, fmt.Errorf("reading credentials: %w", err)
	}
	v := Version{f, fi}
	if !c.Config.equal(BindingOf(u)) {
		return nil, v, nil
	}
	return &c, v, nil
}

// Changed reports whether the Credentials file of upstream is no longer the
// one that v read.
func (s *Store) Changed(upstream string, v Version) bool {
	now, err := os.Stat(s.path(upstream, ".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return v.f != nil
	}
	return err != nil || v.f == nil || !os.SameFile(now, v.fi)
}

// Save replaces the Credentials of upstream with c, under its lock.
func (s *Store) Save(ctx context.Context, upstream string, c *Credentials) error {
	l, err := s.lock(ctx, upstream)
	if err != nil {
		return err
	}
	defer l.unlock()
	return l.save(c)
}

// Delete deletes the Credentials of upstream under its lock, and reports
// whether there were any. The lock file stays.
func (s *Store) Delete(ctx context.Context, upstream string) (bool, error) {
	l, err := s.lock(ctx, upstream)
	if err != nil {
		return false, err
	}
	defer l.unlock()
	err = os.Remove(s.path(upstream, ".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("deleting credentials: %w", err)
	}
	return true, nil
}

// ErrRefreshRejected is what the refresh function given to Renew returns
// when the Authorization Server rejects the refresh token (invalid_grant).
var ErrRefreshRejected = errors.New("the authorization server rejected the refresh token")

// Renew renews the Credentials of u whose access token is stale with
// refresh, under their lock, so that only one process renews them at a
// time, and returns them. It reads them again first: if another process
// has already renewed them, those are returned without a refresh; if they
// are gone, or no longer issued for u as its Config is now, it returns nil.
// When refresh returns ErrRefreshRejected, the Credentials are read again,
// and those are returned if another process that did not wait for the lock
// has rotated the refresh token meanwhile; otherwise the error is. Any
// other error from refresh is returned as it is.
//
// When the renewed Credentials can't be saved, Renew returns them all the
// same, still good for this process, with the error.
func (s *Store) Renew(ctx context.Context, u config.Upstream, stale string, refresh func(context.Context, *Credentials) (*Credentials, error)) (*Credentials, error) {
	l, err := s.lock(ctx, u.Name)
	if err != nil {
		return nil, err
	}
	defer l.unlock()
	// Only once it holds the lock: nothing is rotated while it waits.
	s.mu.Lock()
	s.renewing++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.renewing--; s.renewing == 0 {
			s.idle.Broadcast()
		}
		s.mu.Unlock()
	}()
	c, err := s.Load(u)
	if err != nil || c == nil || c.AccessToken != stale {
		return c, err
	}
	renewed, err := refresh(ctx, c)
	if errors.Is(err, ErrRefreshRejected) {
		again, lerr := s.Load(u)
		if lerr != nil {
			// Unknown, so they are kept.
			return nil, lerr
		}
		if again != nil && again.RefreshToken != c.RefreshToken {
			return again, nil
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return renewed, l.save(renewed)
}

// Wait waits until no Renew that holds the lock is in progress, so that
// Credentials that an Authorization Server has rotated are saved before the
// process exits. A Renew that takes the lock once Wait has returned is not
// waited for. A nil Store has none.
func (s *Store) Wait() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.renewing > 0 {
		s.idle.Wait()
	}
}

// lock is the exclusive lock of one Upstream's Credentials, held by one
// process at a time.
type lock struct {
	s        *Store
	upstream string
	f        *os.File
}

// lock takes the exclusive lock of upstream, waiting for another process to
// release it no longer than lockTimeout and ctx allow.
//
// The lock is on a file of its own, never renamed or deleted, because a
// lock on the Credentials file would stay on the inode that a write
// replaces.
func (s *Store) lock(ctx context.Context, upstream string) (*lock, error) {
	if err := s.mkdir(); err != nil {
		return nil, fmt.Errorf("locking credentials: %w", err)
	}
	f, err := os.OpenFile(s.path(upstream, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("locking credentials: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	// flock can't be interrupted, so it is tried until it succeeds.
	for wait := time.Millisecond; ; wait = min(2*wait, 50*time.Millisecond) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &lock{s: s, upstream: upstream, f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking credentials: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("locking credentials: waiting for another sprut: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
}

// unlock releases l.
func (l *lock) unlock() {
	_ = l.f.Close() // which releases the flock
}

// save replaces the Credentials of l's Upstream with c. It writes a new file
// in place of the old one, so that a reader sees either whole.
func (l *lock) save(c *Credentials) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// CreateTemp makes the file 0600.
	tmp, err := os.CreateTemp(l.s.dir, l.upstream+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing credentials: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // fails once renamed, as it should
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), l.s.path(l.upstream, ".json"))
	}
	if err != nil {
		return fmt.Errorf("writing credentials: %w", err)
	}
	return nil
}

// mkdir makes the Store's directory and its parent, sprut's own, readable
// only by the user, including when they already exist.
func (s *Store) mkdir() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(s.dir), s.dir} {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) path(upstream, ext string) string {
	return filepath.Join(s.dir, upstream+ext)
}
