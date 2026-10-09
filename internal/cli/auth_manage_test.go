package cli_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/marvell/sprut/internal/mcptest"
)

// loginAs logs in to u as the Upstream name of config, with its
// Credentials under state, and returns them.
func loginAs(t *testing.T, u *mcptest.OAuthUpstream, state, config, name string) *loggedIn {
	t.Helper()
	l := &loggedIn{
		OAuthUpstream: u,
		config:        config,
		env:           []string{"XDG_STATE_HOME=" + state},
		creds:         filepath.Join(state, "sprut", "credentials", name+".json"),
	}
	code, stderr := login(t, u, l.env, name, "--no-browser", "-c", config)
	if code != 0 {
		t.Fatalf("auth login %s exit code = %d, want 0\nstderr:\n%s", name, code, stderr)
	}
	return l
}

// makeStale makes the access token of l's Credentials expired, though they
// can still be renewed.
func (l *loggedIn) makeStale(t *testing.T) {
	t.Helper()
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	l.rewriteCreds(t, func(f map[string]any) { f["expiry"] = past })
}

// makeDead makes l's Credentials expired, without a refresh token.
func (l *loggedIn) makeDead(t *testing.T) {
	t.Helper()
	l.makeStale(t)
	l.rewriteCreds(t, func(f map[string]any) { delete(f, "refresh_token") })
}

func TestAuthStatusReportsEachOAuthUpstream(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	state := t.TempDir()
	config := writeConfig(t, map[string]any{
		"none":  map[string]any{"url": u.URL},
		"valid": map[string]any{"url": u.URL},
		"stale": map[string]any{"url": u.URL},
		"dead":  map[string]any{"url": u.URL},
		"local": mcptest.Stdio{}.Entry(t),
	})
	loginAs(t, u, state, config, "valid")
	loginAs(t, u, state, config, "stale").makeStale(t)
	loginAs(t, u, state, config, "dead").makeDead(t)

	code, stdout, stderr := runSprut(t, []string{"XDG_STATE_HOME=" + state}, "auth", "status", "-c", config)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	stamp := `\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\S*`
	want := []string{
		`^dead: the credentials expired and cannot be renewed; ` + regexp.QuoteMeta(loginHint(config, "dead")) + `$`,
		`^none: no credentials$`,
		`^stale: logged in, access token expired ` + stamp + `, renewable$`,
		`^valid: logged in, access token expires ` + stamp + `, renewable$`,
	}
	got := lines(stdout)
	if len(got) != len(want) {
		t.Fatalf("stdout has %d lines, want %d:\n%s", len(got), len(want), stdout)
	}
	for i, pattern := range want {
		if !regexp.MustCompile(pattern).MatchString(got[i]) {
			t.Errorf("line %d = %q, want it to match %q", i, got[i], pattern)
		}
	}
	wantNoSecrets(t, "auth status stdout", stdout, mcptest.Secrets...)
}

func TestConfigFlagGoesBeforeOrAfterTheCommand(t *testing.T) {
	t.Parallel()
	config := writeConfig(t, map[string]any{"remote": map[string]any{"url": "https://remote.invalid/mcp"}})
	env := []string{"XDG_STATE_HOME=" + t.TempDir()}

	for _, args := range [][]string{
		{"-c", config, "auth", "status"},
		{"auth", "-c", config, "status"},
		{"auth", "status", "-c", config},
	} {
		code, stdout, stderr := runSprut(t, env, args...)
		if code != 0 || stdout != "remote: no credentials\n" {
			t.Errorf("sprut %q: exit code = %d, stdout = %q, want 0 listing remote\nstderr:\n%s", args, code, stdout, stderr)
		}
	}
}

func TestAuthLogoutStopsARunningServeFromUsingTheCredentials(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)
	sent := len(l.MCPAuth())

	code, stdout, stderr := runSprut(t, l.env, "auth", "logout", "fake", "-c", l.config)
	if code != 0 || !strings.Contains(stdout, "fake: logged out") {
		t.Fatalf("exit code = %d, stdout = %q, want 0 saying fake is logged out\nstderr:\n%s", code, stdout, stderr)
	}
	wantLoginHint(t, l.config, g.echo(context.Background(), t))
	for _, auth := range l.MCPAuth()[sent:] {
		if auth != "" {
			t.Errorf("a request after the logout carried Authorization %q, want none", auth)
		}
	}
	g.closeAgent(t)
	wantNoCreds(t, l)
	wantMode(t, filepath.Join(filepath.Dir(l.creds), "fake.lock"), 0o600)

	code, stdout, _ = runSprut(t, l.env, "auth", "logout", "fake", "-c", l.config)
	if code != 0 || !strings.Contains(stdout, "fake: no credentials") {
		t.Errorf("second logout: exit code = %d, stdout = %q, want 0 saying there are no credentials", code, stdout)
	}
}

func TestAuthLogoutExits1ForAnUpstreamThatIsNotAnOAuthUpstreamInTheConfig(t *testing.T) {
	t.Parallel()
	config := writeConfig(t, map[string]any{"local": mcptest.Stdio{}.Entry(t)})
	state := t.TempDir()
	for _, name := range []string{"local", "nope"} {
		code, _, stderr := runSprut(t, []string{"XDG_STATE_HOME=" + state}, "auth", "logout", name, "-c", config)
		if code != 1 || !strings.Contains(stderr, `"`+name+`"`) {
			t.Errorf("logout %s: exit code = %d, want 1 naming it\nstderr:\n%s", name, code, stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "sprut")); err == nil {
		t.Error("logout made the state directory")
	}
}

// playLogins waits for l to exit, opening each authorization URL of the
// Authorization Server of u that it prints, as the user would. It returns
// sprut's exit code and stderr.
func (l *loginRun) playLogins(t *testing.T, u *mcptest.OAuthUpstream) (code int, stderr string) {
	t.Helper()
	pattern := authURLPattern(u.Base)
	for opened := 0; ; opened++ {
		var links []string
		eventually(func() bool {
			links = pattern.FindAllString(l.stderr.String(), -1)
			return len(links) > opened || l.exited()
		})
		if len(links) <= opened {
			return l.wait(t)
		}
		l.do(t, http.MethodGet, links[opened])
	}
}

func TestAuthLoginWithoutUpstreamsLogsInOnlyThoseThatNeedIt(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	// Of its own, as the fake honours only the refresh token it issued last.
	renewable := mcptest.StartOAuth(t)
	state := t.TempDir()
	env := []string{"XDG_STATE_HOME=" + state}
	config := writeConfig(t, map[string]any{
		"none":   map[string]any{"url": u.URL},
		"good":   map[string]any{"url": u.URL},
		"stale":  map[string]any{"url": renewable.URL},
		"dead":   map[string]any{"url": u.URL},
		"local":  mcptest.Stdio{}.Entry(t),
		"public": map[string]any{"url": mcptest.StartHTTP(t, "").URL},
	})
	loginAs(t, u, state, config, "good")
	loginAs(t, renewable, state, config, "stale").makeStale(t)
	loginAs(t, u, state, config, "dead").makeDead(t)
	registered := u.Registrations()

	code, stderr := startLogin(t, 30*time.Second, env, "--no-browser", "-c", config, "-v").playLogins(t, u)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	for _, name := range []string{"none", "dead"} {
		if !strings.Contains(stderr, "Logging in to "+name+".") {
			t.Errorf("stderr does not show a login to %s:\n%s", name, stderr)
		}
	}
	for _, name := range []string{"good", "stale", "local", "public"} {
		if strings.Contains(stderr, "Logging in to "+name+".") {
			t.Errorf("stderr shows a login to %s, which needs none:\n%s", name, stderr)
		}
	}
	if n := u.Registrations() - registered; n != 2 {
		t.Errorf("client registrations = %d, want 2", n)
	}
	if n := len(renewable.Refreshes()); n != 1 {
		t.Errorf("refreshes of stale = %d, want 1", n)
	}
	_, stdout, _ := runSprut(t, env, "auth", "status", "-c", config)
	for _, line := range lines(stdout) {
		// public is an OAuth Upstream that never asks for authorization.
		if !strings.HasPrefix(line, "public: ") && !strings.Contains(line, ": logged in, access token expires ") {
			t.Errorf("after the login, auth status says %q, want logged in", line)
		}
	}
	wantNoSecrets(t, "auth login stderr", stderr, mcptest.Secrets...)
}

func TestAuthLoginExits1WhenAnyLoginFailsAndStillLogsInTheOthers(t *testing.T) {
	t.Parallel()
	failing := mcptest.StartOAuth(t)
	failing.RedirectWith(url.Values{"error": {"access_denied"}})
	working := mcptest.StartOAuth(t)
	state := t.TempDir()
	env := []string{"XDG_STATE_HOME=" + state}
	config := writeConfig(t, map[string]any{
		"a": map[string]any{"url": failing.URL},
		"b": map[string]any{"url": working.URL},
	})

	l := startLogin(t, 30*time.Second, env, "--no-browser", "-c", config)
	l.do(t, http.MethodGet, l.mustAuthURL(t, failing).String())
	code, stderr := l.playLogins(t, working)

	if code != 1 || !strings.Contains(stderr, `upstream "a"`) || !strings.Contains(stderr, "access_denied") {
		t.Errorf("exit code = %d, want 1 reporting a's failure\nstderr:\n%s", code, stderr)
	}
	_, stdout, _ := runSprut(t, env, "auth", "status", "-c", config)
	if !strings.Contains(stdout, "b: logged in") || !strings.Contains(stdout, "a: no credentials") {
		t.Errorf("auth status after the login:\n%s\nwant b logged in and a not", stdout)
	}
}

// The Upstream is on localhost, so that it has subdomains. Other origins
// are reached through a proxy, which stands in for them all; only an Upstream
// that sprut can't trust a certificate of would let the test start on
// HTTPS, so the scheme changes the other way.
func TestServeRefusesARedirectToAnotherOriginWithoutSendingTheToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		target func(port string) string
	}{
		{"another host", func(string) string { return "http://elsewhere.invalid/mcp" }},
		{"a subdomain", func(port string) string { return "http://sub.localhost:" + port + "/mcp-moved" }},
		{"another scheme", func(port string) string { return "https://localhost:" + port + "/mcp-moved" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := mcptest.StartOAuthOn(t, "localhost")
			l := logInTo(t, u, 3600)
			proxy := mcptest.StartHTTP(t, "")
			u.RedirectMCP(tc.target(strings.TrimPrefix(u.Base, "http://localhost:")))
			env := append([]string{"HTTP_PROXY=" + proxy.URL, "HTTPS_PROXY=" + proxy.URL}, l.env...)

			g := startGateway(t, l.config, env, "-v")

			g.wantTools(t)
			g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=fake", "redirect")
			g.closeAgent(t)
			for _, h := range proxy.Headers() {
				t.Errorf("the redirect reached the proxy (Authorization %q), want it refused before", h.Get("Authorization"))
			}
			wantNoSecrets(t, "serve stderr", g.stderr.String(), mcptest.Secrets...)
		})
	}
}

func TestServeFollowsARedirectWithinTheUpstreamsOrigin(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	l.RedirectMCP(l.Base + "/mcp-moved")

	g := startGateway(t, l.config, l.env)

	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantEcho(t)
	g.closeAgent(t)
}
