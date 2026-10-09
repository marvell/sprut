package cli_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marvell/sprut/internal/mcptest"
)

func TestAuthLoginStoresCredentialsThatServeSendsFromTheFirstRequest(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	config := writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}})
	state := t.TempDir()
	env := []string{"XDG_STATE_HOME=" + state}
	dir := filepath.Join(state, "sprut", "credentials")
	// Left open by something else: the Login closes it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	stderr := mustLogin(t, u, env, config, "-v")
	if got, want := u.GrantTypes(), []string{"authorization_code", "refresh_token"}; !slices.Equal(got, want) {
		t.Errorf("registered grant types = %v, want %v", got, want)
	}
	if got := u.AppType(); got != "native" {
		t.Errorf("registered application_type = %q, want %q", got, "native")
	}
	wantMode(t, filepath.Join(state, "sprut"), os.ModeDir|0o700)
	wantMode(t, dir, os.ModeDir|0o700)
	wantMode(t, filepath.Join(dir, "fake.json"), 0o600)
	wantMode(t, filepath.Join(dir, "fake.lock"), 0o600)

	before := len(u.MCPAuth())
	g := startGateway(t, config, env, "-v")
	g.wantTools(t, "fake__echo", "fake__fail")
	g.wantEcho(t)
	g.closeAgent(t)

	served := u.MCPAuth()[before:]
	if len(served) == 0 {
		t.Fatal("the Upstream got no requests from serve")
	}
	for i, got := range served {
		if got != "Bearer "+mcptest.AccessToken {
			t.Errorf("request %d from serve: Authorization = %q, want the stored access token", i, got)
		}
	}
	wantNoSecrets(t, "auth login stderr", stderr, mcptest.Secrets...)
	wantNoSecrets(t, "serve stderr", g.stderr.String(), mcptest.Secrets...)
}

func TestAuthLoginPrintsTheIssuerAndScopesBeforeTheURL(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	config := writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}})

	stderr := mustLogin(t, u, []string{"XDG_STATE_HOME=" + t.TempDir()}, config)
	at := authURLPattern(u.Base).FindStringIndex(stderr)
	before := stderr[:at[0]]
	for _, want := range []string{"authorization server: " + u.Base + "\n", "scopes: " + mcptest.Scope + "\n"} {
		if !strings.Contains(before, want) {
			t.Errorf("stderr before the authorization URL lacks %q:\n%s", want, stderr)
		}
	}
}

func TestAuthLoginIgnoresCallbacksThatAreNotTheOneItWaitsFor(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	config := writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}})
	l := startLogin(t, 30*time.Second, []string{"XDG_STATE_HOME=" + t.TempDir()}, "fake", "--no-browser", "-c", config)
	link := l.mustAuthURL(t, u)
	callback, err := url.Parse(link.Query().Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	valid := callback.Query()
	valid.Set("code", mcptest.Code)
	valid.Set("state", link.Query().Get("state"))
	wrongState := callback.Query()
	wrongState.Set("code", mcptest.Code)
	wrongState.Set("state", "forged")

	for _, tc := range []struct {
		name, method, path, query string
	}{
		{"wrong path", http.MethodGet, "/other", valid.Encode()},
		{"wrong method", http.MethodPost, callback.Path, valid.Encode()},
		{"wrong state", http.MethodGet, callback.Path, wrongState.Encode()},
		{"wrong state with an error", http.MethodGet, callback.Path, "error=access_denied&state=forged"},
	} {
		target := *callback
		target.Path, target.RawQuery = tc.path, tc.query
		resp := l.do(t, tc.method, target.String())
		if resp.StatusCode < 400 {
			t.Errorf("%s: callback answered %s, want an error status", tc.name, resp.Status)
		}
		if l.exited() {
			code, _ := l.wait(t)
			t.Fatalf("%s: auth login exited %d", tc.name, code)
		}
	}
	if resp := l.do(t, http.MethodGet, link.String()); resp.StatusCode != http.StatusOK {
		t.Errorf("the valid callback answered %s", resp.Status)
	}
	if code, _ := l.wait(t); code != 0 {
		t.Fatalf("auth login exit code = %d, want 0", code)
	}
}

func TestAuthLoginChecksTheIssuerOfTheAuthorizationResponse(t *testing.T) {
	t.Parallel()
	const description = "fake-error-description-0b7c"
	for _, tc := range []struct {
		name      string
		advertise bool
		redirect  func(u *mcptest.OAuthUpstream) url.Values
		wantCode  int
	}{
		{"matching iss", false, func(u *mcptest.OAuthUpstream) url.Values {
			return url.Values{"code": {mcptest.Code}, "iss": {u.Base}}
		}, 0},
		{"mismatched iss", false, func(*mcptest.OAuthUpstream) url.Values {
			return url.Values{"code": {mcptest.Code}, "iss": {"https://evil.example"}}
		}, 1},
		{"error with a mismatched iss", false, func(*mcptest.OAuthUpstream) url.Values {
			return url.Values{"error": {"access_denied"}, "error_description": {description}, "iss": {"https://evil.example"}}
		}, 1},
		{"advertised iss missing from an error", true, func(*mcptest.OAuthUpstream) url.Values {
			return url.Values{"error": {"access_denied"}, "error_description": {description}}
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u := mcptest.StartOAuth(t)
			if tc.advertise {
				u.AdvertiseIss()
			}
			u.RedirectWith(tc.redirect(u))
			config := writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}})
			state := t.TempDir()

			l := startLogin(t, 30*time.Second, []string{"XDG_STATE_HOME=" + state}, "fake", "--no-browser", "-c", config)
			if link := l.authURL(t, u); link != nil {
				l.do(t, http.MethodGet, link.String())
			}

			code, stderr := l.wait(t)

			if code != tc.wantCode {
				t.Fatalf("auth login exit code = %d, want %d", code, tc.wantCode)
			}
			if code == 0 {
				return
			}
			if _, err := os.Stat(filepath.Join(state, "sprut", "credentials", "fake.json")); err == nil {
				t.Error("Credentials were written")
			}
			if !strings.Contains(stderr, "issuer") {
				t.Errorf("stderr does not say the response's issuer is wrong:\n%s", stderr)
			}
			wantNoSecrets(t, "auth login stderr", stderr, "access_denied", description, "evil.example")
		})
	}
}

// Only the test's deadline on the Login's context is short: the Login's
// own 5 minutes are the same error.
func TestAuthLoginWithoutACallbackTimesOutWithExit1(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	config := writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}})
	l := startLogin(t, 3*time.Second, []string{"XDG_STATE_HOME=" + t.TempDir()}, "fake", "--no-browser", "-c", config)
	l.mustAuthURL(t, u)

	code, stderr := l.wait(t)

	if code != 1 || !strings.Contains(stderr, "no callback from the browser") {
		t.Errorf("exit code = %d, want 1 saying no callback came", code)
	}
}

func TestServeSkipsOAuthUpstreamWithoutCredentialsWithALoginHint(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	config := writeConfig(t, map[string]any{
		"fake":  map[string]any{"url": u.URL},
		"other": mcptest.Stdio{}.Entry(t),
	})
	env := []string{"XDG_STATE_HOME=" + t.TempDir()}

	g := startGateway(t, config, env)
	g.wantTools(t, "other__echo", "other__fail")
	g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=fake", `hint="`+loginHint(config, "fake")+`"`)
	instructions := g.Agent.InitializeResult().Instructions
	if !strings.Contains(instructions, "- fake: ") || !strings.Contains(instructions, loginHint(config, "fake")) {
		t.Errorf("instructions do not list fake with the login hint:\n%s", instructions)
	}
	g.closeAgent(t)

	code, _, _ := runSprut(t, env, "serve", "--dry-run", "-c", config)
	if code != 1 {
		t.Errorf("--dry-run exit code = %d, want 1", code)
	}
}

// The Agent runs sprut with its own directory and environment, so the hint
// names the Config by an absolute path, quoted for the shell, unless it is
// where sprut looks by default.
func TestTheLoginHintNamesTheConfigAsAShellElsewhereFindsIt(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	upstreams := map[string]any{"fake": map[string]any{"url": u.URL}}
	state := "XDG_STATE_HOME=" + t.TempDir()

	spaced := filepath.Join(t.TempDir(), "my configs", "config.json")
	writeConfigAt(t, spaced, upstreams)
	configHome := t.TempDir()
	writeConfigAt(t, filepath.Join(configHome, "sprut", "config.json"), upstreams)
	abs := writeConfig(t, upstreams)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		env  []string
		args []string
		want string
	}{
		{name: "default location", env: []string{"XDG_CONFIG_HOME=" + configHome}, want: "run: sprut auth login fake"},
		{name: "relative -c", args: []string{"-c", rel}, want: loginHint(abs, "fake")},
		{name: "SPRUT_CONFIG with a space", env: []string{"SPRUT_CONFIG=" + spaced}, want: loginHint("'"+spaced+"'", "fake")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"serve", "--dry-run"}, tc.args...)
			code, _, stderr := runSprut(t, append(tc.env, state), args...)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr, `hint="`+tc.want+`"`) {
				t.Errorf("stderr lacks the hint %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// The fake is reached through a proxy, so that its URL can be plain HTTP
// on a host that is not a loopback address.
func TestOAuthOverPlainHTTPToANonLoopbackHostIsRefused(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	config := writeConfig(t, map[string]any{"remote": map[string]any{"url": "http://remote.invalid/mcp"}})
	env := []string{"HTTP_PROXY=" + u.Base, "XDG_STATE_HOME=" + t.TempDir()}

	t.Run("serve", func(t *testing.T) {
		t.Parallel()
		g := startGateway(t, config, env)
		g.wantTools(t)
		g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=remote", "OAuth requires HTTPS")
		g.closeAgent(t)
	})
	t.Run("auth login", func(t *testing.T) {
		t.Parallel()
		code, _, stderr := runSprut(t, env, "auth", "login", "remote", "--no-browser", "-c", config)
		if code != 1 || !strings.Contains(stderr, "OAuth requires HTTPS") {
			t.Errorf("exit code = %d, want 1 saying OAuth requires HTTPS", code)
		}
	})
}

func TestAuthLoginExits1ForAnUpstreamThatIsNotAnOAuthUpstream(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	for _, tc := range []struct {
		name   string
		entry  map[string]any
		reason string
	}{
		{"stdio", mcptest.Stdio{}.Entry(t), "OAuth is only for HTTP upstreams"},
		{"static header", map[string]any{"url": u.URL, "headers": map[string]any{"authorization": "Bearer x"}}, "Authorization header"},
		{"never answers 401", map[string]any{"url": mcptest.StartHTTP(t, "").URL}, "did not ask for authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := writeConfig(t, map[string]any{"up": tc.entry})
			state := t.TempDir()

			code, _, stderr := runSprut(t, []string{"XDG_STATE_HOME=" + state}, "auth", "login", "up", "--no-browser", "-c", config)

			if code != 1 || !strings.Contains(stderr, tc.reason) {
				t.Errorf("exit code = %d, want 1 with a reason containing %q", code, tc.reason)
			}
			if _, err := os.Stat(filepath.Join(state, "sprut", "credentials", "up.json")); err == nil {
				t.Error("Credentials were written")
			}
		})
	}
}

func TestAuthLoginExits1ForAnUpstreamNotInTheConfig(t *testing.T) {
	t.Parallel()
	config := writeConfig(t, map[string]any{"up": mcptest.Stdio{}.Entry(t)})

	code, _, stderr := runSprut(t, nil, "auth", "login", "nope", "-c", config)

	if code != 1 || !strings.Contains(stderr, `"nope"`) {
		t.Errorf("exit code = %d, want 1 naming the upstream", code)
	}
}

func TestAuthReadsTheConfigThatSPRUTCONFIGNames(t *testing.T) {
	t.Parallel()
	config := writeConfig(t, map[string]any{"up": mcptest.Stdio{}.Entry(t)})

	code, _, stderr := runSprut(t, []string{"SPRUT_CONFIG=" + config}, "auth", "login", "up")

	// Only the Config there knows up, and that it is stdio.
	if code != 1 || !strings.Contains(stderr, "OAuth is only for HTTP upstreams") {
		t.Errorf("exit code = %d, want 1 saying up is not an HTTP upstream", code)
	}
}

func TestAuthUsageErrorsExit2(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"auth"},
		{"auth", "bogus"},
		{"auth", "login", "--bogus", "a"},
		{"auth", "logout"},
		{"auth", "logout", "a", "b"},
		{"auth", "status", "a"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			code, stdout, _ := runSprut(t, nil, args...)

			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}
