package cli_test

import (
	"regexp"
	"testing"

	"github.com/marvell/sprut/internal/mcptest"
)

func TestUpstreamListReportsTheCredentialsOfEachOAuthUpstream(t *testing.T) {
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

	code, stdout, _ := runSprut(t, []string{"XDG_STATE_HOME=" + state}, "upstream", "list", "-c", config)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	stamp := `\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\S*`
	want := []string{
		`^NAME +TRANSPORT +TARGET +STATE +AUTH$`,
		`^dead +http +\S+ +enabled +the credentials expired and cannot be renewed; ` + regexp.QuoteMeta(loginHint(config, "dead")) + `$`,
		`^local +stdio +.* +enabled +-$`,
		`^none +http +\S+ +enabled +no credentials$`,
		`^stale +http +\S+ +enabled +logged in, access token expired ` + stamp + `, renewable$`,
		`^valid +http +\S+ +enabled +logged in, access token expires ` + stamp + `, renewable$`,
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
	wantNoSecrets(t, "upstream list stdout", stdout, mcptest.Secrets...)
}
