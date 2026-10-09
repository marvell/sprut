package credentials_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/oauth2"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
)

// linear is the OAuth Upstream that the tests keep Credentials for.
var linear = config.Upstream{Name: "linear", Transport: config.HTTP, URL: "https://mcp.linear.app/mcp"}

// stateHome returns a fresh $XDG_STATE_HOME.
func stateHome(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// openStore returns the Store under stateHome. Two Stores opened on one
// stateHome stand for two sprut processes: each takes the lock through a
// file of its own, and flock tells them apart as it tells processes apart.
func openStore(t *testing.T, stateHome string) *credentials.Store {
	t.Helper()
	s, err := credentials.NewStore(config.LookupIn([]string{"XDG_STATE_HOME=" + stateHome}))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// credsFile is the path of u's Credentials file under stateHome.
func credsFile(stateHome string, u config.Upstream) string {
	return filepath.Join(stateHome, "sprut", "credentials", u.Name+".json")
}

// loggedIn returns the Credentials of a Login to u, the first that the
// fake Authorization Server issues: access-1 and refresh-1, expiring in
// expiresIn.
func loggedIn(u config.Upstream, expiresIn time.Duration) *credentials.Credentials {
	c := &credentials.Credentials{
		TokenURL: "https://auth.example/token",
		Resource: u.URL,
		Issuer:   "https://auth.example",
		Config:   credentials.BindingOf(u),
	}
	c.AccessToken, c.RefreshToken, c.TokenType = "access-1", "refresh-1", "Bearer"
	c.Expiry = time.Now().Add(expiresIn).Round(0)
	return c
}

// save saves c as u's Credentials in s, failing the test if it can't.
func save(t *testing.T, s *credentials.Store, u config.Upstream, c *credentials.Credentials) {
	t.Helper()
	if err := s.Save(t.Context(), u.Name, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// load returns u's Credentials in s, failing the test if it can't read
// them.
func load(t *testing.T, s *credentials.Store, u config.Upstream) *credentials.Credentials {
	t.Helper()
	c, err := s.Load(u)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

// writeUnlocked replaces u's Credentials file under stateHome with c by a
// rename, as a process that does not wait for the lock would.
func writeUnlocked(t *testing.T, stateHome string, u config.Upstream, c *credentials.Credentials) {
	t.Helper()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	tmp := credsFile(stateHome, u) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, credsFile(stateHome, u)); err != nil {
		t.Fatal(err)
	}
}

// authServer is a credentials.Refresh standing in for an Authorization
// Server that rotates the refresh token: its nth refresh issues access-n+1
// and refresh-n+1, each access token living an hour.
type authServer struct {
	mu    sync.Mutex
	calls int
	err   error         // returned in place of new Credentials, if set
	hold  chan struct{} // if set, each refresh waits for it to close
	// entered, if set, gets the Credentials of each refresh as it begins.
	entered chan *credentials.Credentials
}

func (a *authServer) refresh(ctx context.Context, c *credentials.Credentials, _ string) (*credentials.Credentials, error) {
	a.mu.Lock()
	a.calls++
	n, err, hold, entered := a.calls, a.err, a.hold, a.entered
	a.mu.Unlock()
	if entered != nil {
		entered <- c
	}
	if hold != nil {
		<-hold
	}
	if err != nil {
		return nil, err
	}
	renewed := *c
	renewed.AccessToken = fmt.Sprintf("access-%d", n+1)
	renewed.RefreshToken = fmt.Sprintf("refresh-%d", n+1)
	renewed.Expiry = time.Now().Add(time.Hour).Round(0)
	return &renewed, nil
}

// Calls returns how many refreshes it got.
func (a *authServer) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// holding returns an authServer whose refreshes wait until release is
// called, and report on entered as they begin.
func holding(t *testing.T) (a *authServer, release func()) {
	t.Helper()
	a = &authServer{hold: make(chan struct{}), entered: make(chan *credentials.Credentials, 10)}
	release = sync.OnceFunc(func() { close(a.hold) })
	t.Cleanup(release)
	return a, release
}

// waitEntered waits for a's next refresh to begin.
func (a *authServer) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-a.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no refresh began")
	}
}

// source returns the Source of u's Credentials in s, refreshing with a and
// logging to the test output.
func source(t *testing.T, s *credentials.Store, u config.Upstream, a *authServer) *credentials.Source {
	t.Helper()
	return s.Source(u, a.refresh, slog.New(slog.NewTextHandler(t.Output(), nil)))
}

func TestStoreSavesCredentialsReadableOnlyByTheUser(t *testing.T) {
	t.Parallel()
	home := stateHome(t)
	s := openStore(t, home)
	want := loggedIn(linear, time.Hour)
	save(t, s, linear, want)

	if diff := cmp.Diff(want, load(t, s, linear), cmpopts.IgnoreUnexported(oauth2.Token{})); diff != "" {
		t.Errorf("loaded Credentials (-saved +loaded):\n%s", diff)
	}
	for path, mode := range map[string]os.FileMode{
		credsFile(home, linear):                     0o600,
		filepath.Join(home, "sprut"):                0o700 | os.ModeDir,
		filepath.Join(home, "sprut", "credentials"): 0o700 | os.ModeDir,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != mode {
			t.Errorf("%s: mode %v, want %v", path, fi.Mode(), mode)
		}
	}
}

func TestStoreLoadsNothingWithoutALogin(t *testing.T) {
	t.Parallel()
	if c := load(t, openStore(t, stateHome(t)), linear); c != nil {
		t.Errorf("Load = %+v, want nil", c)
	}
}

func TestStoreLoadsCredentialsOnlyForTheConfigTheyWereIssuedFor(t *testing.T) {
	t.Parallel()
	withOAuth := func(o config.OAuth) config.Upstream {
		u := linear
		u.OAuth = &o
		return u
	}
	tests := []struct {
		name          string
		login, now    config.Upstream
		wantKeepCreds bool
	}{
		{name: "unchanged", login: linear, now: linear, wantKeepCreds: true},
		{name: "another url", login: linear, now: config.Upstream{Name: "linear", URL: "https://elsewhere.example/mcp"}},
		{name: "added oauth.clientId", login: linear, now: withOAuth(config.OAuth{ClientID: "sprut"})},
		{name: "another oauth.clientId", login: withOAuth(config.OAuth{ClientID: "a"}), now: withOAuth(config.OAuth{ClientID: "b"})},
		{
			name:          "another oauth.clientSecret",
			login:         withOAuth(config.OAuth{ClientID: "a", ClientSecret: "old"}),
			now:           withOAuth(config.OAuth{ClientID: "a", ClientSecret: "rotated"}),
			wantKeepCreds: true,
		},
		{name: "added oauth.scopes", login: linear, now: withOAuth(config.OAuth{Scopes: []string{"read"}})},
		{name: "empty oauth.scopes", login: linear, now: withOAuth(config.OAuth{Scopes: []string{}})},
		{name: "another oauth.scopes", login: withOAuth(config.OAuth{Scopes: []string{"read"}}), now: withOAuth(config.OAuth{Scopes: []string{"read", "write"}})},
		{
			name:          "oauth.scopes reordered",
			login:         withOAuth(config.OAuth{Scopes: []string{"read", "write"}}),
			now:           withOAuth(config.OAuth{Scopes: []string{"write", "read"}}),
			wantKeepCreds: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t, stateHome(t))
			save(t, s, linear, loggedIn(tt.login, time.Hour))

			if got := load(t, s, tt.now); (got != nil) != tt.wantKeepCreds {
				t.Errorf("Load = %+v, want Credentials: %v", got, tt.wantKeepCreds)
			}
		})
	}
}

func TestStoreReportsMalformedCredentials(t *testing.T) {
	t.Parallel()
	home := stateHome(t)
	s := openStore(t, home)
	save(t, s, linear, loggedIn(linear, time.Hour))
	if err := os.WriteFile(credsFile(home, linear), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Load(linear); err == nil {
		t.Error("Load of a malformed file returned no error")
	}
}

func TestStoreDeleteReportsWhetherThereWereCredentials(t *testing.T) {
	t.Parallel()
	s := openStore(t, stateHome(t))
	save(t, s, linear, loggedIn(linear, time.Hour))

	for _, want := range []bool{true, false} {
		deleted, err := s.Delete(t.Context(), linear.Name)
		if err != nil || deleted != want {
			t.Errorf("Delete = %v, %v, want %v, nil", deleted, err, want)
		}
	}
	if c := load(t, s, linear); c != nil {
		t.Errorf("Load after Delete = %+v, want nil", c)
	}
}

func TestNewStoreFallsBackToHomeAndFailsWithoutOne(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	s, err := credentials.NewStore(config.LookupIn([]string{"HOME=" + home, "XDG_STATE_HOME=relative/is/ignored"}))
	if err != nil {
		t.Fatal(err)
	}
	save(t, s, linear, loggedIn(linear, time.Hour))
	if _, err := os.Stat(filepath.Join(home, ".local", "state", "sprut", "credentials", "linear.json")); err != nil {
		t.Errorf("Credentials not under ~/.local/state: %v", err)
	}

	if _, err := credentials.NewStore(config.LookupIn(nil)); !errors.Is(err, credentials.ErrNoHome) {
		t.Errorf("NewStore without HOME: err = %v, want ErrNoHome", err)
	}
}

// One process renewing holds the lock against every other, even once the
// Credentials file has been replaced: the lock is on a file of its own,
// because a rename gives the Credentials file a new inode (ADR-0003).
func TestStoreLockHoldsAcrossProcessesAndRenames(t *testing.T) {
	t.Parallel()
	home := stateHome(t)
	one, other := openStore(t, home), openStore(t, home)
	save(t, one, linear, loggedIn(linear, time.Minute))
	as, release := holding(t)
	renewing := make(chan error, 1)
	go func() {
		_, err := source(t, one, linear, as).Token(t.Context())
		renewing <- err
	}()
	as.waitEntered(t) // one now holds the lock

	writeUnlocked(t, home, linear, loggedIn(linear, time.Hour))

	// Meant to fail: it must not get the lock while one holds it.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := other.Save(ctx, linear.Name, loggedIn(linear, time.Hour)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Save while another process renews: err = %v, want it to wait for the lock until the deadline", err)
	}

	release()
	if err := <-renewing; err != nil {
		t.Errorf("Token: %v", err)
	}
	save(t, other, linear, loggedIn(linear, time.Hour)) // the lock is free again
}
