package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAuthLoginStoresCredentialsThatServeSendsFromTheFirstRequest(t *testing.T) {
	t.Parallel()
	u := startOAuthUpstream(t)
	config := writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}})
	state := t.TempDir()
	env := []string{"XDG_STATE_HOME=" + state}

	code, stderr := login(t, u, env, "fake", "--no-browser", "-v", "-c", config)
	if code != 0 {
		t.Fatalf("auth login exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if got, want := u.GrantTypes(), []string{"authorization_code", "refresh_token"}; !slices.Equal(got, want) {
		t.Errorf("registered grant types = %v, want %v", got, want)
	}
	dir := filepath.Join(state, "sprut", "credentials")
	wantMode(t, dir, os.ModeDir|0o700)
	wantMode(t, filepath.Join(dir, "fake.json"), 0o600)
	wantMode(t, filepath.Join(dir, "fake.lock"), 0o600)

	before := len(u.MCPAuth())
	g := startGateway(t, config, env, "-v")
	g.wantTools(t, "fake__echo", "fake__fail")
	res, err := g.Agent.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "fake__echo", Arguments: map[string]any{"text": "hi"},
	})
	if err != nil {
		t.Fatalf("calling fake__echo: %v", err)
	}
	if got := resultText(t, res); got != "echo: hi" {
		t.Errorf("fake__echo = %q, want %q", got, "echo: hi")
	}
	g.closeAgent(t)

	served := u.MCPAuth()[before:]
	if len(served) == 0 {
		t.Fatal("the Upstream got no requests from serve")
	}
	for i, got := range served {
		if got != "Bearer "+fakeAccessToken {
			t.Errorf("request %d from serve: Authorization = %q, want the stored access token", i, got)
		}
	}
	wantNoSecrets(t, "auth login stderr", stderr, fakeSecrets...)
	wantNoSecrets(t, "serve stderr", g.stderr.String(), fakeSecrets...)
}

// wantMode checks that the file at path exists with mode want.
func wantMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	if got := info.Mode() & (os.ModeDir | os.ModePerm); got != want {
		t.Errorf("%s: mode = %v, want %v", path, got, want)
	}
}

// wantNoSecrets checks that out, named what, holds none of secrets.
func wantNoSecrets(t *testing.T, what, out string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Errorf("%s contains the secret %q:\n%s", what, secret, out)
		}
	}
}

func TestServeSkipsOAuthUpstreamWithoutCredentialsWithALoginHint(t *testing.T) {
	t.Parallel()
	u := startOAuthUpstream(t)
	config := writeConfig(t, map[string]any{
		"fake":  map[string]any{"url": u.URL},
		"other": fakeUpstreamEntry(t),
	})
	env := []string{"XDG_STATE_HOME=" + t.TempDir()}

	g := startGateway(t, config, env)
	g.wantTools(t, "other__echo", "other__fail")
	g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=fake", `hint="run: sprut auth login fake"`)
	instructions := g.Agent.InitializeResult().Instructions
	if !strings.Contains(instructions, "- fake: ") || !strings.Contains(instructions, "run: sprut auth login fake") {
		t.Errorf("instructions do not list fake with the login hint:\n%s", instructions)
	}
	g.closeAgent(t)

	code, _, stderr := runSprut(t, append([]string{envFakeUpstream + "=1"}, env...), "serve", "--dry-run", "-c", config)
	if code != 1 {
		t.Errorf("--dry-run exit code = %d, want 1\nstderr:\n%s", code, stderr)
	}
}

// The fake is reached through a proxy, so that its URL can be plain HTTP
// on a host that is not a loopback address.
func TestOAuthOverPlainHTTPToANonLoopbackHostIsRefused(t *testing.T) {
	t.Parallel()
	u := startOAuthUpstream(t)
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
			t.Errorf("exit code = %d, want 1 saying OAuth requires HTTPS\nstderr:\n%s", code, stderr)
		}
	})
}

func TestAuthLoginExits1ForAnUpstreamThatIsNotAnOAuthUpstream(t *testing.T) {
	t.Parallel()
	u := startOAuthUpstream(t)
	for _, tc := range []struct {
		name   string
		entry  map[string]any
		reason string
	}{
		{"stdio", fakeUpstreamEntry(t), "OAuth is only for HTTP upstreams"},
		{"static header", map[string]any{"url": u.URL, "headers": map[string]any{"authorization": "Bearer x"}}, "Authorization header"},
		{"never answers 401", map[string]any{"url": startHTTPUpstream(t, "").URL}, "did not ask for authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := writeConfig(t, map[string]any{"up": tc.entry})
			state := t.TempDir()

			code, _, stderr := runSprut(t, []string{"XDG_STATE_HOME=" + state}, "auth", "login", "up", "--no-browser", "-c", config)

			if code != 1 || !strings.Contains(stderr, tc.reason) {
				t.Errorf("exit code = %d, want 1 with a reason containing %q\nstderr:\n%s", code, tc.reason, stderr)
			}
			if _, err := os.Stat(filepath.Join(state, "sprut", "credentials", "up.json")); err == nil {
				t.Error("Credentials were written")
			}
		})
	}
}

func TestAuthLoginExits1ForAnUpstreamNotInTheConfig(t *testing.T) {
	t.Parallel()
	config := writeConfig(t, map[string]any{"up": fakeUpstreamEntry(t)})

	code, _, stderr := runSprut(t, nil, "auth", "login", "nope", "-c", config)

	if code != 1 || !strings.Contains(stderr, `"nope"`) {
		t.Errorf("exit code = %d, want 1 naming the upstream\nstderr:\n%s", code, stderr)
	}
}

func TestAuthReadsTheConfigThatSPRUTCONFIGNames(t *testing.T) {
	t.Parallel()
	config := writeConfig(t, map[string]any{"up": fakeUpstreamEntry(t)})

	code, _, stderr := runSprut(t, []string{"SPRUT_CONFIG=" + config}, "auth", "login", "up")

	// Only the Config there knows up, and that it is stdio.
	if code != 1 || !strings.Contains(stderr, "OAuth is only for HTTP upstreams") {
		t.Errorf("exit code = %d, want 1 saying up is not an HTTP upstream\nstderr:\n%s", code, stderr)
	}
}

func TestAuthUsageErrorsExit2(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"auth"},
		{"auth", "bogus"},
		{"auth", "login"},
		{"auth", "login", "a", "b"},
		{"auth", "login", "--bogus", "a"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runSprut(t, nil, args...)

			if code != 2 {
				t.Errorf("exit code = %d, want 2\nstderr:\n%s", code, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}
