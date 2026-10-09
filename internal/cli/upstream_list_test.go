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
	wantRows(t, stdout,
		`^dead +http +\S+ +enabled +the credentials expired and cannot be renewed; `+regexp.QuoteMeta(loginHint(config, "dead"))+`$`,
		`^local +stdio +.* +enabled +-$`,
		`^none +http +\S+ +enabled +no credentials$`,
		`^stale +http +\S+ +enabled +logged in, access token expired `+stamp+`, renewable$`,
		`^valid +http +\S+ +enabled +logged in, access token expires `+stamp+`, renewable$`,
	)
	wantNoSecrets(t, "upstream list stdout", stdout, mcptest.Secrets...)
}

func TestUpstreamListCheckReportsWhetherEachHTTPUpstreamWorksBeforeAndAfterALogin(t *testing.T) {
	t.Parallel()
	u := mcptest.StartOAuth(t)
	plain := mcptest.StartHTTP(t, "")
	state := t.TempDir()
	env := []string{"XDG_STATE_HOME=" + state}
	config := writeConfig(t, map[string]any{
		"oauth": map[string]any{"url": u.URL},
		"dead":  map[string]any{"url": u.URL},
		"plain": map[string]any{"url": plain.URL},
	})
	loginAs(t, u, state, config, "dead").makeDead(t)

	code, stdout, _ := runSprut(t, env, "upstream", "list", "--check", "-c", config)
	if code != 1 {
		t.Errorf("before the Login: exit code = %d, want 1", code)
	}
	wantRows(t, stdout,
		`^dead +http +\S+ +needs login +the credentials expired`,
		`^oauth +http +\S+ +needs login +no credentials$`,
		`^plain +http +\S+ +ok \(2 tools\) +no credentials$`,
	)

	loginAs(t, u, state, config, "oauth")
	loginAs(t, u, state, config, "dead")
	code, stdout, _ = runSprut(t, env, "upstream", "list", "--check", "-c", config)
	if code != 0 {
		t.Errorf("after the Login: exit code = %d, want 0", code)
	}
	wantRows(t, stdout,
		`^dead +http +\S+ +ok \(2 tools\) +logged in`,
		`^oauth +http +\S+ +ok \(2 tools\) +logged in`,
		`^plain +http +\S+ +ok \(2 tools\) +no credentials$`,
	)
	wantNoSecrets(t, "upstream list --check stdout", stdout, mcptest.Secrets...)
}

func TestUpstreamListCheckReportsAnUpstreamThatRejectsItsCredentialsAsNeedingALogin(t *testing.T) {
	t.Parallel()
	l := logIn(t, 3600)
	l.RevokeAccessTokens()
	l.FailRefresh("invalid_grant")

	code, stdout, _ := runSprut(t, l.env, "upstream", "list", "--check", "-c", l.config)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	wantRows(t, stdout, `^fake +http +\S+ +needs login +logged in`)
}

// wantRows checks that the rows of a table that sprut upstream list
// printed to stdout, after its header, match want, in order.
func wantRows(t *testing.T, stdout string, want ...string) {
	t.Helper()
	got := lines(stdout)
	if len(got) != len(want)+1 || !regexp.MustCompile(`^NAME +TRANSPORT +TARGET +STATE +AUTH$`).MatchString(got[0]) {
		t.Fatalf("stdout = %q, want a header and %d rows", stdout, len(want))
	}
	for i, pattern := range want {
		if !regexp.MustCompile(pattern).MatchString(got[i+1]) {
			t.Errorf("row %d = %q, want it to match %q", i, got[i+1], pattern)
		}
	}
}

func TestUpstreamListCheckStartsUpstreamsConcurrently(t *testing.T) {
	t.Parallel()
	// Each Upstream waits for all three to be running before it answers, so
	// checked one after another the first would time out.
	meet := mcptest.Stdio{Rendezvous: &mcptest.Rendezvous{N: 3, Dir: t.TempDir()}}
	config := writeConfig(t, map[string]any{
		"one":   meet.Entry(t),
		"two":   meet.Entry(t),
		"three": meet.Entry(t),
	})

	code, stdout, _ := runSprut(t, []string{"XDG_STATE_HOME=" + t.TempDir()}, "upstream", "list", "--check", "--startup-timeout", "5s", "-c", config)

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	wantRows(t, stdout,
		`^one +stdio +.* +ok \(2 tools\) +-$`,
		`^three +stdio +.* +ok \(2 tools\) +-$`,
		`^two +stdio +.* +ok \(2 tools\) +-$`,
	)
}
