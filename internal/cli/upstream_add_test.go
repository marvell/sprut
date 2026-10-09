package cli_test

import (
	"strings"
	"testing"

	"github.com/marvell/sprut/internal/mcptest"
)

func TestUpstreamAddSaysAnOAuthUpstreamNeedsALoginWithoutStartingOne(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	state := t.TempDir()
	env := []string{"XDG_STATE_HOME=" + state}
	config := writeConfig(t, map[string]any{})

	code, stdout, stderr := runSprut(t, env, "upstream", "add", "-c", config, "fake", u.URL)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if want := `upstream "fake": needs login; ` + loginHint(config, "fake") + "\n"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
	if want := "added upstream \"fake\"\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if got := u.GrantTypes(); len(got) != 0 {
		t.Errorf("a client registered with grant types %q, want none: add must not start a Login", got)
	}

	loginAs(t, u, state, config, "fake")
	code, stdout, _ = runSprut(t, env, "upstream", "add", "--force", "-c", config, "fake", u.URL)
	if code != 0 {
		t.Errorf("after the Login: exit code = %d, want 0", code)
	}
	if want := "upstream \"fake\": ok (2 tools)\n"; !strings.Contains(stdout, want) {
		t.Errorf("after the Login: stdout = %q, want it to contain %q", stdout, want)
	}
}
