package cli_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

// callAll calls fake__echo through every g at once, and checks that each
// call echoes.
func callAll(t *testing.T, gs ...*gateway) {
	t.Helper()
	var wg sync.WaitGroup
	for _, g := range gs {
		wg.Go(func() {
			res, err := g.callEcho(t.Context())
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
	wantOneRefreshPerRotation(t, l.OAuthUpstream)
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
			rotated := l.Issue() // as a refresh by another process would
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
	wantLoginHint(t, l.config, g.echo(t.Context(), t))

	l.FailRefresh("")
	mustLogin(t, l.OAuthUpstream, l.env, l.config)
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
		wantLoginHint(t, l.config, g.echo(t.Context(), t))

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

func TestServeGivesUpWaitingForTheLockWithTheCall(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)
	before := l.readCreds(t)

	unlock := l.lockCreds(t)
	l.RevokeAccessTokens()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if res, err := g.callEcho(ctx); err == nil {
		t.Errorf("fake__echo = %v, want the call to end with the Agent's timeout", toJSON(t, res))
	}
	l.wantCredsUnchanged(t, before)

	unlock()
	g.wantEcho(t)
	g.closeAgent(t)
}
