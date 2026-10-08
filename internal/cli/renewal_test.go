package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// loggedIn is an OAuth Upstream, "fake", that `sprut auth login` has stored
// Credentials for.
type loggedIn struct {
	*oauthUpstream
	config string
	env    []string
	creds  string // the path of the Credentials file
}

// logIn starts a fake OAuth Upstream whose access tokens live for expiresIn
// seconds, and logs in to it.
func logIn(t *testing.T, expiresIn int) *loggedIn {
	t.Helper()
	return logInTo(t, startOAuthUpstream(t), expiresIn)
}

// logInTo logs in to u, whose access tokens live for expiresIn seconds.
func logInTo(t *testing.T, u *oauthUpstream, expiresIn int) *loggedIn {
	t.Helper()
	u.ExpireIn(expiresIn)
	state := t.TempDir()
	l := &loggedIn{
		oauthUpstream: u,
		config:        writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}}),
		env:           []string{"XDG_STATE_HOME=" + state},
		creds:         filepath.Join(state, "sprut", "credentials", "fake.json"),
	}
	mustLogin(t, u, l.env, l.config)
	return l
}

// readCreds returns the content of the Credentials file.
func (l *loggedIn) readCreds(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(l.creds)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// wantCredsUnchanged checks that the Credentials file still holds before.
func (l *loggedIn) wantCredsUnchanged(t *testing.T, before string) {
	t.Helper()
	if got := l.readCreds(t); got != before {
		t.Errorf("Credentials changed:\nbefore: %s\nafter:  %s", before, got)
	}
}

// leakySecrets are fakeSecrets and what the fake Authorization Server says
// around them in an error_description.
var leakySecrets = append(slices.Clone(fakeSecrets), "of client")

// callEcho calls fake__echo under ctx.
func (g *gateway) callEcho(ctx context.Context) (*mcp.CallToolResult, error) {
	return g.Agent.CallTool(ctx, &mcp.CallToolParams{Name: "fake__echo", Arguments: map[string]any{"text": "hi"}})
}

// echo calls fake__echo and returns its result.
func (g *gateway) echo(ctx context.Context, t *testing.T) *mcp.CallToolResult {
	t.Helper()
	res, err := g.callEcho(ctx)
	if err != nil {
		t.Fatalf("calling fake__echo: %v", err)
	}
	return res
}

// wantLoginHint checks that res is an isError result with the Login hint.
func wantLoginHint(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	text := resultText(t, res)
	if !res.IsError || !strings.Contains(text, "run: sprut auth login fake") {
		t.Errorf("fake__echo = %q (isError %v), want an isError result with the login hint", text, res.IsError)
	}
	wantNoSecrets(t, "tool result", text, leakySecrets...)
}

// wantEcho checks that a call to fake__echo succeeds.
func (g *gateway) wantEcho(t *testing.T) {
	t.Helper()
	res := g.echo(context.Background(), t)
	if res.IsError || resultText(t, res) != "echo: hi" {
		t.Fatalf("fake__echo = %v, want it to echo\nstderr:\n%s", toJSON(t, res), g.stderr)
	}
}

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
	wantNoSecrets(t, "serve stderr", g.stderr.String(), fakeSecrets...)
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
	if creds := l.readCreds(t); !strings.Contains(creds, fakeAccessToken+"-") {
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
		wantLoginHint(t, g.echo(context.Background(), t))

		g.closeAgent(t)
		wantNoSecrets(t, "serve stderr", g.stderr.String(), leakySecrets...)
	})
	t.Run("at startup", func(t *testing.T) {
		t.Parallel()
		l := logIn(t, 60) // renewed at startup
		l.FailRefresh("invalid_grant")

		g := startGateway(t, l.config, l.env, "-v")

		g.wantTools(t)
		g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=fake", `hint="run: sprut auth login fake"`)
		g.closeAgent(t)
		wantNoSecrets(t, "serve stderr", g.stderr.String(), leakySecrets...)
	})
}

func TestServeSkipsAnUpstreamWhoseCredentialsExpiredWithoutARefreshToken(t *testing.T) {
	t.Parallel()
	u := startOAuthUpstream(t)
	u.IssueNoRefreshToken()
	l := logInTo(t, u, 1)
	time.Sleep(1100 * time.Millisecond) // past its expiry

	g := startGateway(t, l.config, l.env)

	g.wantTools(t)
	g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=fake", `hint="run: sprut auth login fake"`)
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
	g.closeAgent(t)
	l.wantCredsUnchanged(t, before)
}

func TestServeSendsNoRefreshWhenTheUpstreamNamesAnotherIssuer(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)

	l.NameIssuer("https://other.example")
	l.RevokeAccessTokens()
	wantLoginHint(t, g.echo(context.Background(), t))

	if n := len(l.Refreshes()); n != 0 {
		t.Errorf("refreshes = %d, want none", n)
	}
	g.closeAgent(t)
}
