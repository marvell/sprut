package cli_test

import (
	"context"
	"testing"
)

// The fake Authorization Server echoes every secret it knows in its errors,
// as an OAuth error_description and as a body that is not JSON, at each
// step that talks to it; sprut reports only the error code and the HTTP
// status.
func TestNoSecretAnAuthorizationServerEchoesReachesTheOutput(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"json", "text"} {
		t.Run("client registration, "+how, func(t *testing.T) {
			t.Parallel()
			u := startOAuthUpstream(t)
			u.FailRegistration(how)
			config := writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}})

			code, _, stderr := runSprut(t, []string{"XDG_STATE_HOME=" + t.TempDir()},
				"auth", "login", "fake", "--no-browser", "-v", "-c", config)

			if code != 1 {
				t.Errorf("exit code = %d, want 1\nstderr:\n%s", code, stderr)
			}
			wantNoSecrets(t, "auth login stderr", stderr, leakySecrets...)
		})
		t.Run("code exchange, "+how, func(t *testing.T) {
			t.Parallel()
			u := startOAuthUpstream(t)
			u.FailCodeExchange(how)
			config := writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}})

			code, stderr := login(t, u, []string{"XDG_STATE_HOME=" + t.TempDir()}, "fake", "--no-browser", "-v", "-c", config)

			if code != 1 {
				t.Errorf("exit code = %d, want 1\nstderr:\n%s", code, stderr)
			}
			wantNoSecrets(t, "auth login stderr", stderr, leakySecrets...)
		})
	}
	for _, how := range []string{"5xx", "text", "invalid_grant"} {
		t.Run("refresh at startup, "+how, func(t *testing.T) {
			t.Parallel()
			// Always within the 5 minutes before expiry, so serve renews
			// first.
			l := logIn(t, 2)
			l.FailRefresh(how)

			g := startGateway(t, l.config, l.env, "-v")

			g.wantTools(t)
			wantNoSecrets(t, "instructions", g.Agent.InitializeResult().Instructions, leakySecrets...)
			g.closeAgent(t)
			wantNoSecrets(t, "serve stderr", g.stderr.String(), leakySecrets...)
		})
		t.Run("refresh mid-session, "+how, func(t *testing.T) {
			t.Parallel()
			l := logIn(t, 3600)
			g := startGateway(t, l.config, l.env, "-v")
			g.wantEcho(t)
			l.FailRefresh(how)
			l.RevokeAccessTokens()

			res := g.echo(context.Background(), t)

			if !res.IsError {
				t.Errorf("fake__echo = %v, want an isError result", toJSON(t, res))
			}
			wantNoSecrets(t, "tool result", resultText(t, res), leakySecrets...)
			g.closeAgent(t)
			wantNoSecrets(t, "serve stderr", g.stderr.String(), leakySecrets...)
		})
	}
}
