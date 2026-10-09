package cli_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marvell/sprut/internal/mcptest"
)

func TestServeRenewsCredentialsWhenTheUpstreamAnswers401(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env, "-v")
	g.wantEcho(t)

	l.RevokeAccessTokens()
	g.wantEcho(t)

	if n := len(l.Refreshes()); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
	g.wantLogLine(t, "DEBUG", `msg="credentials renewed"`, "upstream=fake", "expires=")
	g.closeAgent(t)
	wantNoSecrets(t, "serve stderr", g.stderr.String(), mcptest.Secrets...)
}

// Access tokens that live for 2s are always within the 5 minutes before
// expiry in which they are renewed, so every request renews first.
func TestServeRenewsCredentialsBeforeTheyExpireWithTheResource(t *testing.T) {
	t.Parallel()
	l := logIn(t, 2)
	rejected := l.Rejected()
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)

	time.Sleep(2200 * time.Millisecond) // past the expiry of every token so far
	g.wantEcho(t)

	if n := l.Rejected() - rejected; n != 0 {
		t.Errorf("the Upstream answered 401 to %d requests from serve, want none", n)
	}
	refreshes := l.Refreshes()
	if len(refreshes) < 2 {
		t.Errorf("refreshes = %d, want one at least for each call", len(refreshes))
	}
	for i, form := range refreshes {
		if got := form.Get("resource"); got != l.URL {
			t.Errorf("refresh %d: resource = %q, want %q", i, got, l.URL)
		}
	}
	if creds := l.readCreds(t); !strings.Contains(creds, mcptest.AccessToken+"-") {
		t.Errorf("the Credentials file does not hold a renewed access token:\n%s", creds)
	}
	g.closeAgent(t)
}

func TestServeRenewsOnceForConcurrentCalls(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)

	l.RevokeAccessTokens()
	callAll(t, slices.Repeat([]*gateway{g}, 8)...)

	if n := len(l.Refreshes()); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
	g.closeAgent(t)
}

func TestServeReportsCredentialsTheAuthorizationServerRejectsWithALoginHint(t *testing.T) {
	t.Parallel()
	t.Run("mid-session", func(t *testing.T) {
		t.Parallel()
		l := logIn(t, 3600)
		g := startGateway(t, l.config, l.env, "-v")
		g.wantEcho(t)

		l.FailRefresh("invalid_grant")
		l.RevokeAccessTokens()
		wantLoginHint(t, l.config, g.echo(context.Background(), t))

		g.closeAgent(t)
		wantNoSecrets(t, "serve stderr", g.stderr.String(), leakySecrets...)
	})
	t.Run("at startup", func(t *testing.T) {
		t.Parallel()
		l := logIn(t, 60) // renewed at startup
		l.FailRefresh("invalid_grant")

		g := startGateway(t, l.config, l.env, "-v")

		g.wantTools(t)
		g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=fake", `hint="`+loginHint(l.config, "fake")+`"`)
		g.closeAgent(t)
		wantNoSecrets(t, "serve stderr", g.stderr.String(), leakySecrets...)
	})
}

func TestServeSkipsAnUpstreamWhoseCredentialsExpiredWithoutARefreshToken(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	u.IssueNoRefreshToken()
	l := logInTo(t, u, 1)
	time.Sleep(1100 * time.Millisecond) // past its expiry

	g := startGateway(t, l.config, l.env)

	g.wantTools(t)
	g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=fake", `hint="`+loginHint(l.config, "fake")+`"`)
	if n := len(l.Refreshes()); n != 0 {
		t.Errorf("refreshes = %d, want none", n)
	}
	g.closeAgent(t)
}

func TestServeKeepsCredentialsWhenRenewalFailsOnTheWay(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"5xx", "drop", "hang"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			l := logIn(t, 3600)
			g := startGateway(t, l.config, l.env, "-v")
			g.wantEcho(t)
			before := l.readCreds(t)

			l.FailRefresh(how)
			l.RevokeAccessTokens()
			// The Agent gives up on a hanging call, well before the 30s that
			// the token request is allowed.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			res, err := g.callEcho(ctx)
			switch {
			case how == "hang" && err == nil:
				t.Errorf("fake__echo = %v, want the call to end with the Agent's timeout", toJSON(t, res))
			case how != "hang" && (err != nil || !res.IsError):
				t.Errorf("fake__echo = %v, %v; want an isError result", res, err)
			case how != "hang":
				text := resultText(t, res)
				if strings.Contains(text, "sprut auth login") {
					t.Errorf("fake__echo = %q, want no login hint", text)
				}
				wantNoSecrets(t, "tool result", text, leakySecrets...)
			}
			l.wantCredsUnchanged(t, before)

			// Only the call failed: once the Authorization Server is back,
			// so is the Upstream.
			l.FailRefresh("")
			l.Release()
			if !eventually(func() bool {
				res, err := g.callEcho(context.Background())
				return err == nil && !res.IsError
			}) {
				t.Errorf("fake__echo still fails once refreshes work\nstderr:\n%s", g.stderr)
			}
			g.closeAgent(t)
			wantNoSecrets(t, "serve stderr", g.stderr.String(), leakySecrets...)
		})
	}
}

func TestServeCountsRenewalAtStartupTowardTheStartupTimeout(t *testing.T) {
	t.Parallel()
	l := logIn(t, 60) // renewed at startup
	l.FailRefresh("hang")
	before := l.readCreds(t)

	g := startGateway(t, l.config, l.env, "--startup-timeout", "1s")

	g.wantTools(t)
	g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=fake", "startup timed out")
	// The Gateway waits for the renewal before it exits.
	l.Release()
	g.closeAgent(t)
	l.wantCredsUnchanged(t, before)
}

// A renewal can't be dropped once it has begun: the Authorization Server
// may already have rotated the refresh token, and only the Credentials
// file keeps the new one past the process. One that an Upstream abandoned
// at startup began has no session left to wait for it.
func TestServeSavesARenewalInProgressBeforeItExits(t *testing.T) {
	t.Parallel()
	l := logIn(t, 60) // renewed at startup
	refreshing := make(chan struct{})
	var once sync.Once
	l.OnRefresh(func() {
		once.Do(func() { close(refreshing) })
		<-l.Released() // until Release, which the fake's cleanup also calls
	})

	g := startGateway(t, l.config, l.env, "--startup-timeout", "1s")
	g.wantTools(t)
	select {
	case <-refreshing:
	case <-time.After(10 * time.Second):
		t.Fatalf("no refresh request\nstderr:\n%s", g.stderr)
	}

	// Closing the Agent's side returns only once the Gateway has exited.
	go func() { _ = g.Agent.Close() }()
	// Without the wait, the Gateway still exits only after the SDK's 5s for
	// the notifications/cancelled of the abandoned start; an exit later
	// than this wait goes undetected.
	select {
	case <-g.exit:
		t.Fatalf("Gateway exited with a renewal in progress\nstderr:\n%s", g.stderr)
	case <-time.After(7 * time.Second):
	}
	l.Release()
	g.wantCleanExit(t, "EOF on stdin")
	if creds := l.readCreds(t); !strings.Contains(creds, `"refresh_token":"`+mcptest.RefreshToken+`-2"`) {
		t.Errorf("Credentials = %s, want the rotated refresh token", creds)
	}
}

func TestServeSendsNoRefreshWhenTheUpstreamNamesAnotherIssuer(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)

	l.NameIssuer("https://other.example")
	l.RevokeAccessTokens()
	wantLoginHint(t, l.config, g.echo(context.Background(), t))

	if n := len(l.Refreshes()); n != 0 {
		t.Errorf("refreshes = %d, want none", n)
	}
	g.closeAgent(t)
}
