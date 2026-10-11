package gateway_test

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
	"github.com/marvell/sprut/internal/gateway"
)

// remedy is the remedy that the Auths of these tests give.
func remedy(u config.Upstream) string { return "log in to " + u.Name }

func TestLogoutDeletesTheCredentialsAndKeepsTheLock(t *testing.T) {
	t.Parallel()
	c := openConfig(t, map[string]any{"oauth": map[string]any{"url": "https://oauth.invalid/mcp"}})
	store, state := newStoreIn(t)
	save(t, store, c, "oauth", oauth2.Token{AccessToken: "a"})
	a := gateway.OpenAuths([]string{"XDG_STATE_HOME=" + state}, remedy).For(upstream(t, c, "oauth"))

	had, err := a.Logout(t.Context())
	if err != nil || !had {
		t.Fatalf("Logout = %t, %v; want true, nil", had, err)
	}
	dir := filepath.Join(state, "sprut", "credentials")
	if _, err := os.Stat(filepath.Join(dir, "oauth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the Credentials file is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "oauth.lock")); err != nil {
		t.Errorf("the lock file is gone: %v", err)
	}
	if got := a.Status(); got.State != gateway.AuthNoCredentials {
		t.Errorf("status after Logout = %+v, want no Credentials", got)
	}

	had, err = a.Logout(t.Context())
	if err != nil || had {
		t.Errorf("second Logout = %t, %v; want false, nil", had, err)
	}
}

func TestAnUpstreamThatIsNotAnOAuthUpstreamHasNoCredentialsAndRefusesLoginAndLogoutWithWhy(t *testing.T) {
	t.Parallel()
	c := openConfig(t, map[string]any{
		"local":  map[string]any{"command": "fake-upstream"},
		"static": map[string]any{"url": "https://static.invalid/mcp", "headers": map[string]any{"authorization": "Bearer x"}},
		"plain":  map[string]any{"url": "http://plain.invalid/mcp"},
	})
	_, state := newStoreIn(t)
	auths := gateway.OpenAuths([]string{"XDG_STATE_HOME=" + state}, remedy)
	for name, reason := range map[string]string{
		"local":  "uses stdio, and OAuth is only for HTTP upstreams",
		"static": "sends an Authorization header from the config, so it does not use OAuth",
		"plain":  "OAuth requires HTTPS",
	} {
		a := auths.For(upstream(t, c, name))
		if got := a.Status(); got.State != gateway.AuthNotOAuth {
			t.Errorf("%s: status = %+v, want not an OAuth Upstream", name, got)
		}
		if _, err := a.Logout(t.Context()); err == nil || err.Error() != reason {
			t.Errorf("%s: Logout error = %v, want %q", name, err, reason)
		}
		if err := a.Login(t.Context(), false, io.Discard, "test", slog.New(slog.DiscardHandler)); err == nil || err.Error() != reason {
			t.Errorf("%s: Login error = %v, want %q", name, err, reason)
		}
	}
}

func TestADisabledOAuthUpstreamIsJudgedAsIfEnabled(t *testing.T) {
	t.Parallel()
	c := openConfig(t, map[string]any{"paused": map[string]any{"url": "https://paused.invalid/mcp", "disabled": true}})
	store, state := newStoreIn(t)
	save(t, store, c, "paused", oauth2.Token{AccessToken: "a"})
	a := gateway.OpenAuths([]string{"XDG_STATE_HOME=" + state}, remedy).For(upstream(t, c, "paused"))

	if got := a.Status(); got.State != gateway.AuthLoggedIn {
		t.Errorf("status = %+v, want logged in", got)
	}
	if had, err := a.Logout(t.Context()); err != nil || !had {
		t.Errorf("Logout = %t, %v; want true, nil", had, err)
	}
}

// Whether an Upstream is an OAuth Upstream comes first: one that isn't
// needs no Store, and says why it isn't rather than that there is none.
func TestWithoutAHomeDirectoryOnlyAnOAuthUpstreamFailsForWantOfAStore(t *testing.T) {
	t.Parallel()
	c := openConfig(t, map[string]any{
		"oauth": map[string]any{"url": "https://oauth.invalid/mcp"},
		"local": map[string]any{"command": "fake-upstream"},
	})
	auths := gateway.OpenAuths(nil, remedy)

	if err := auths.Located(); !errors.Is(err, credentials.ErrNoHome) {
		t.Errorf("Located = %v, want %v", err, credentials.ErrNoHome)
	}
	if _, err := auths.Forget(t.Context(), "oauth"); !errors.Is(err, credentials.ErrNoHome) {
		t.Errorf("Forget error = %v, want %v", err, credentials.ErrNoHome)
	}
	oauth := auths.For(upstream(t, c, "oauth"))
	if got := oauth.Status(); got.State != gateway.AuthUnreadable || !errors.Is(got.Err, credentials.ErrNoHome) {
		t.Errorf("oauth: status = %+v, want unreadable with %v", got, credentials.ErrNoHome)
	}
	if _, err := oauth.Logout(t.Context()); !errors.Is(err, credentials.ErrNoHome) {
		t.Errorf("oauth: Logout error = %v, want %v", err, credentials.ErrNoHome)
	}
	if err := oauth.Login(t.Context(), false, io.Discard, "test", slog.New(slog.DiscardHandler)); !errors.Is(err, credentials.ErrNoHome) {
		t.Errorf("oauth: Login error = %v, want %v", err, credentials.ErrNoHome)
	}

	local := auths.For(upstream(t, c, "local"))
	if got := local.Status(); got.State != gateway.AuthNotOAuth {
		t.Errorf("local: status = %+v, want not an OAuth Upstream", got)
	}
	if _, err := local.Logout(t.Context()); err == nil || !strings.Contains(err.Error(), "only for HTTP upstreams") {
		t.Errorf("local: Logout error = %v, want why it is not an OAuth Upstream", err)
	}
}

func TestForgetDeletesCredentialsWhateverTheEntry(t *testing.T) {
	t.Parallel()
	c := openConfig(t, map[string]any{"oauth": map[string]any{"url": "https://oauth.invalid/mcp"}})
	store, state := newStoreIn(t)
	save(t, store, c, "oauth", oauth2.Token{AccessToken: "a"})
	auths := gateway.OpenAuths([]string{"XDG_STATE_HOME=" + state}, remedy)

	if err := auths.Located(); err != nil {
		t.Errorf("Located = %v, want nil", err)
	}
	if had, err := auths.Forget(t.Context(), "oauth"); err != nil || !had {
		t.Errorf("Forget = %t, %v; want true, nil", had, err)
	}
	if had, err := auths.Forget(t.Context(), "oauth"); err != nil || had {
		t.Errorf("second Forget = %t, %v; want false, nil", had, err)
	}
}

func TestDeadCredentialsGiveTheRemedyTheAuthsWereOpenedWith(t *testing.T) {
	t.Parallel()
	c := openConfig(t, map[string]any{"dead": map[string]any{"url": "https://dead.invalid/mcp"}})
	store, state := newStoreIn(t)
	save(t, store, c, "dead", oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(-time.Hour)})
	a := gateway.OpenAuths([]string{"XDG_STATE_HOME=" + state}, remedy).For(upstream(t, c, "dead"))

	if got := a.Status(); got.State != gateway.AuthDead || got.Reason != credentials.ReasonExpired || got.Remedy != "log in to dead" {
		t.Errorf("status = %+v, want dead, expired, with the remedy %q", got, "log in to dead")
	}
}

// upstream is the Upstream name of c, resolved.
func upstream(t *testing.T, c *config.Config, name string) config.Upstream {
	t.Helper()
	u, err := c.Upstream(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
