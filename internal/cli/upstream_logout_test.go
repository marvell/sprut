package cli_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestUpstreamLogoutStopsARunningServeFromUsingTheCredentials(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)
	sent := len(l.MCPAuth())

	code, stdout, _ := runSprut(t, l.env, "upstream", "logout", "fake", "-c", l.config)
	if code != 0 || !strings.Contains(stdout, "fake: logged out") {
		t.Fatalf("exit code = %d, stdout = %q, want 0 saying fake is logged out", code, stdout)
	}
	wantLoginHint(t, l.config, g.echo(t.Context(), t))
	for _, auth := range l.MCPAuth()[sent:] {
		if auth != "" {
			t.Errorf("a request after the logout carried Authorization %q, want none", auth)
		}
	}
	g.closeAgent(t)
	wantNoCreds(t, l)
	wantMode(t, filepath.Join(filepath.Dir(l.creds), "fake.lock"), 0o600)

	code, stdout, _ = runSprut(t, l.env, "upstream", "logout", "fake", "-c", l.config)
	if code != 0 || !strings.Contains(stdout, "fake: no credentials") {
		t.Errorf("second logout: exit code = %d, stdout = %q, want 0 saying there are no credentials", code, stdout)
	}
}
