// Package credentials stores the Credentials of each OAuth Upstream in a
// file of its own, written only under a lock on a sibling file (ADR 0003).
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

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
	// Authorization Server.
	Resource string `json:"resource"`
	Issuer   string `json:"issuer"`
}

// Store is the directory holding the Credentials of every Upstream.
type Store struct{ dir string }

// NewStore returns the Store under $XDG_STATE_HOME/sprut/credentials, or
// ~/.local/state/sprut/credentials.
func NewStore(lookupEnv config.LookupEnv) (*Store, error) {
	if dir, ok := config.XDGDir(lookupEnv, "XDG_STATE_HOME", filepath.Join(".local", "state")); ok {
		return &Store{dir: filepath.Join(dir, "sprut", "credentials")}, nil
	}
	return nil, errors.New("cannot locate the credentials: HOME is not set")
}

// Load returns the Credentials of upstream, or nil if it has none.
func (s *Store) Load(upstream string) (*Credentials, error) {
	data, err := os.ReadFile(s.path(upstream, ".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("reading credentials %s: %w", s.path(upstream, ".json"), err)
	}
	return &c, nil
}

// Save replaces the Credentials of upstream with c. It holds the lock of
// upstream while it writes, and writes a new file in place of the old one,
// so that a reader sees either whole.
func (s *Store) Save(upstream string, c *Credentials) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := s.mkdir(); err != nil {
		return fmt.Errorf("writing credentials: %w", err)
	}
	unlock, err := s.lock(upstream)
	if err != nil {
		return err
	}
	defer unlock()

	// CreateTemp makes the file 0600.
	tmp, err := os.CreateTemp(s.dir, upstream+".*.tmp")
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
		err = os.Rename(tmp.Name(), s.path(upstream, ".json"))
	}
	if err != nil {
		return fmt.Errorf("writing credentials: %w", err)
	}
	return nil
}

// lock takes the exclusive lock of upstream, and returns what releases it.
// The lock is on a file of its own, never renamed or deleted, because a
// lock on the Credentials file would stay on the inode that a write
// replaces.
func (s *Store) lock(upstream string) (unlock func(), err error) {
	f, err := os.OpenFile(s.path(upstream, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("locking credentials: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking credentials: %w", err)
	}
	// Closing the file releases the lock.
	return func() { _ = f.Close() }, nil
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
