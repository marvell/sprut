package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wantOneRefreshPerRotation checks that no refresh token was sent to the
// Authorization Server twice: a second refresh with one would mean that
// two processes renewed the same Credentials.
func wantOneRefreshPerRotation(t *testing.T, u *oauthUpstream) {
	t.Helper()
	seen := map[string]bool{}
	for _, form := range u.Refreshes() {
		token := form.Get("refresh_token")
		if seen[token] {
			t.Errorf("refresh token %q was sent more than once", token)
		}
		seen[token] = true
	}
}

// wantEchoed checks that a call to fake__echo that returned res and err
// succeeded.
func wantEchoed(t *testing.T, res *mcp.CallToolResult, err error) {
	t.Helper()
	if err != nil || res.IsError {
		t.Errorf("fake__echo = %v, %v; want it to echo", res, err)
	}
}

// callAll calls fake__echo through every g at once, and checks that each
// call echoes.
func callAll(t *testing.T, gs ...*gateway) {
	t.Helper()
	var wg sync.WaitGroup
	for _, g := range gs {
		wg.Go(func() {
			res, err := g.callEcho(context.Background())
			wantEchoed(t, res, err)
		})
	}
	wg.Wait()
}

func TestServeProcessesSharingCredentialsRenewThemOnce(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	a := startGateway(t, l.config, l.env)
	b := startGateway(t, l.config, l.env)
	a.wantEcho(t)
	b.wantEcho(t)

	// The refresh is answered only once both have been rejected, so that
	// both renew at once.
	rejected := l.Rejected()
	l.OnRefresh(func() { eventually(func() bool { return l.Rejected() >= rejected+2 }) })
	l.RevokeAccessTokens()
	callAll(t, a, b)

	if n := len(l.Refreshes()); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
	a.wantEcho(t)
	b.wantEcho(t)
	a.closeAgent(t)
	b.closeAgent(t)
}

// rewriteCreds replaces the Credentials file with a new one, as a write by
// sprut would, after edit changes its JSON fields. It takes no lock.
func (l *loggedIn) rewriteCreds(t *testing.T, edit func(fields map[string]any)) {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(l.readCreds(t)), &fields); err != nil {
		t.Fatal(err)
	}
	edit(fields)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	tmp := l.creds + ".test"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, l.creds); err != nil {
		t.Fatal(err)
	}
}

// lockCreds takes the lock that sprut renews the Credentials under, as
// another sprut process would, and returns what releases it.
func (l *loggedIn) lockCreds(t *testing.T) (unlock func()) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(filepath.Dir(l.creds), "fake.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	unlock = func() { _ = f.Close() }
	t.Cleanup(unlock)
	return unlock
}

// startEcho calls fake__echo through g in the background, once the Upstream
// rejects the current access token, and returns where its result arrives
// once the call has been rejected.
func (l *loggedIn) startEcho(t *testing.T, g *gateway) <-chan *callOutcome {
	t.Helper()
	rejected := l.Rejected()
	done := make(chan *callOutcome, 1)
	go func() {
		res, err := g.callEcho(context.Background())
		done <- &callOutcome{res, err}
	}()
	if !eventually(func() bool { return l.Rejected() > rejected }) {
		t.Fatalf("the call was not rejected\nstderr:\n%s", g.stderr)
	}
	return done
}

type callOutcome struct {
	res *mcp.CallToolResult
	err error
}

// A rename that replaces the Credentials file while one process renews
// them must not let another process renew them too: it would if the lock
// were on the Credentials file, whose inode the rename replaces.
func TestServeRenewsOnceWhileAnotherWritesTheCredentialsWithARename(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	a := startGateway(t, l.config, l.env)
	c := startGateway(t, l.config, l.env)
	a.wantEcho(t)
	c.wantEcho(t)

	// a's refresh is held at the Authorization Server, its lock taken.
	arrived, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	l.OnRefresh(func() {
		first.Do(func() {
			close(arrived)
			<-release
		})
	})
	l.RevokeAccessTokens()
	aDone := l.startEcho(t, a)
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("a did not renew")
	}

	l.rewriteCreds(t, func(map[string]any) {})
	cDone := l.startEcho(t, c)
	// For c to reach the lock. If it is late, a's renewal is over, and the
	// test no longer tells a lock on the replaced inode apart.
	time.Sleep(200 * time.Millisecond)
	close(release)

	for _, done := range []<-chan *callOutcome{aDone, cDone} {
		r := <-done
		wantEchoed(t, r.res, r.err)
	}
	wantOneRefreshPerRotation(t, l.oauthUpstream)
	a.closeAgent(t)
	c.closeAgent(t)
}

// A process that renewed without waiting for the lock leaves the refresh
// token that another process sends stale: the Authorization Server answers
// invalid_grant, and the Credentials it renewed are the ones to use.
func TestServeKeepsCredentialsThatAnotherProcessRotatedOnInvalidGrant(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)

	var first sync.Once
	l.OnRefresh(func() {
		first.Do(func() {
			rotated := l.issue() // as a refresh by another process would
			l.rewriteCreds(t, func(fields map[string]any) {
				fields["access_token"] = rotated["access_token"]
				fields["refresh_token"] = rotated["refresh_token"]
			})
		})
	})
	l.RevokeAccessTokens()
	g.wantEcho(t)
	g.wantEcho(t)

	if n := len(l.Refreshes()); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
	g.closeAgent(t)
}

func TestServePicksUpALoginWithoutARestart(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)

	l.FailRefresh("invalid_grant")
	l.RevokeAccessTokens()
	wantLoginHint(t, l.config, g.echo(context.Background(), t))

	l.FailRefresh("")
	mustLogin(t, l.oauthUpstream, l.env, l.config)
	g.wantEcho(t)
	g.closeAgent(t)
}

func TestServeDropsCredentialsWhoseFileIsDeleted(t *testing.T) {
	t.Parallel()
	t.Run("between requests", func(t *testing.T) {
		t.Parallel()
		l := logIn(t, 3600)
		g := startGateway(t, l.config, l.env)
		g.wantEcho(t)
		sent := len(l.MCPAuth())

		if err := os.Remove(l.creds); err != nil {
			t.Fatal(err)
		}
		wantLoginHint(t, l.config, g.echo(context.Background(), t))

		for _, auth := range l.MCPAuth()[sent:] {
			if auth != "" {
				t.Errorf("a request after the deletion carried Authorization %q, want none", auth)
			}
		}
		g.closeAgent(t)
		wantNoCreds(t, l)
	})
	t.Run("while a renewal waits for the lock", func(t *testing.T) {
		t.Parallel()
		l := logIn(t, 3600)
		g := startGateway(t, l.config, l.env)
		g.wantEcho(t)

		// As a logout does: the file is deleted under the lock.
		unlock := l.lockCreds(t)
		l.RevokeAccessTokens()
		done := l.startEcho(t, g)
		// For the renewal to reach the lock. If it is late, it finds the file
		// gone before it locks, which the test can't tell apart.
		time.Sleep(200 * time.Millisecond)
		if err := os.Remove(l.creds); err != nil {
			t.Fatal(err)
		}
		unlock()

		r := <-done
		if r.err != nil {
			t.Fatalf("fake__echo: %v", r.err)
		}
		wantLoginHint(t, l.config, r.res)
		if n := len(l.Refreshes()); n != 0 {
			t.Errorf("refreshes = %d, want none", n)
		}
		g.closeAgent(t)
		wantNoCreds(t, l)
	})
}

// wantNoCreds checks that the Credentials file does not exist.
func wantNoCreds(t *testing.T, l *loggedIn) {
	t.Helper()
	if _, err := os.Stat(l.creds); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the Credentials file exists (%v), want it gone", err)
	}
}

func TestServeGivesUpWaitingForTheLockWithTheCall(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)
	before := l.readCreds(t)

	unlock := l.lockCreds(t)
	l.RevokeAccessTokens()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if res, err := g.callEcho(ctx); err == nil {
		t.Errorf("fake__echo = %v, want the call to end with the Agent's timeout", toJSON(t, res))
	}
	l.wantCredsUnchanged(t, before)

	unlock()
	g.wantEcho(t)
	g.closeAgent(t)
}
