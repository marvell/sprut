package gateway_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/marvell/sprut/internal/config"
	"github.com/marvell/sprut/internal/credentials"
	"github.com/marvell/sprut/internal/gateway"
	"github.com/marvell/sprut/internal/mcptest"
)

func TestReportsSayWhetherTheGatewayStartsEachEntry(t *testing.T) {
	t.Parallel()
	c := openConfig(t, map[string]any{
		"on":     map[string]any{"command": "fake-upstream"},
		"off":    map[string]any{"command": "fake-upstream", "disabled": true},
		"unset":  map[string]any{"command": "fake-upstream", "env": map[string]any{"TOKEN": "${SPRUT_TEST_UNSET}"}},
		"legacy": map[string]any{"type": "sse", "url": "https://legacy.invalid/sse"},
	})

	reports := gateway.Reports(t.Context(), c.Entries, newStore(t), nil)

	want := map[string]struct {
		start   gateway.StartState
		skipped string
	}{
		"on":     {gateway.StartEnabled, ""},
		"off":    {gateway.StartDisabled, ""},
		"unset":  {gateway.StartSkipped, "unset variable SPRUT_TEST_UNSET"},
		"legacy": {gateway.StartSkipped, "SSE transport is not supported"},
	}
	if len(reports) != len(want) {
		t.Fatalf("got %d reports, want %d", len(reports), len(want))
	}
	for _, r := range reports {
		w := want[r.Entry.Name]
		if r.Start != w.start || r.Entry.Skipped != w.skipped {
			t.Errorf("%s: start = %v, skipped %q; want %v, skipped %q", r.Entry.Name, r.Start, r.Entry.Skipped, w.start, w.skipped)
		}
		if r.Health != nil {
			t.Errorf("%s: health = %+v without a check, want none", r.Entry.Name, r.Health)
		}
	}
}

func TestReportsDescribeTheCredentialsOfEachOAuthUpstream(t *testing.T) {
	t.Parallel()
	c := openConfig(t, map[string]any{
		"local":    map[string]any{"command": "fake-upstream"},
		"static":   map[string]any{"url": "https://static.invalid/mcp", "headers": map[string]any{"authorization": "Bearer x"}},
		"plain":    map[string]any{"url": "http://plain.invalid/mcp"},
		"secret":   map[string]any{"url": "https://secret.invalid/mcp?key=${SECRET_KEY}"},
		"none":     map[string]any{"url": "https://none.invalid/mcp"},
		"valid":    map[string]any{"url": "https://valid.invalid/mcp"},
		"stale":    map[string]any{"url": "https://stale.invalid/mcp"},
		"forever":  map[string]any{"url": "https://forever.invalid/mcp"},
		"dead":     map[string]any{"url": "https://dead.invalid/mcp"},
		"paused":   map[string]any{"url": "https://paused.invalid/mcp", "disabled": true},
		"broken":   map[string]any{"url": "https://broken.invalid/mcp"},
		"loopback": map[string]any{"url": "http://127.0.0.1:1/mcp"},
	})
	store, state := newStoreIn(t)
	future, past := time.Now().Add(time.Hour).Round(time.Second), time.Now().Add(-time.Hour).Round(time.Second)
	save(t, store, c, "valid", oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: future})
	save(t, store, c, "stale", oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: past})
	save(t, store, c, "forever", oauth2.Token{AccessToken: "a"})
	save(t, store, c, "dead", oauth2.Token{AccessToken: "a", Expiry: past})
	save(t, store, c, "paused", oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: future})
	if err := os.WriteFile(filepath.Join(state, "sprut", "credentials", "broken.json"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	reports := gateway.Reports(t.Context(), c.Entries, store, nil)

	want := map[string]gateway.Auth{
		"local":    {State: gateway.AuthNotOAuth},
		"static":   {State: gateway.AuthNotOAuth},
		"plain":    {State: gateway.AuthNotOAuth},
		"secret":   {State: gateway.AuthNotOAuth},
		"none":     {State: gateway.AuthNoCredentials},
		"valid":    {State: gateway.AuthLoggedIn, Expiry: future, Renewable: true},
		"stale":    {State: gateway.AuthLoggedIn, Expiry: past, Expired: true, Renewable: true},
		"forever":  {State: gateway.AuthLoggedIn},
		"dead":     {State: gateway.AuthDead, Reason: credentials.ReasonExpired, Remedy: "run: sprut upstream login -c " + c.Path + " dead"},
		"paused":   {State: gateway.AuthLoggedIn, Expiry: future, Renewable: true},
		"loopback": {State: gateway.AuthNoCredentials},
	}
	if len(reports) != len(want)+1 {
		t.Fatalf("got %d reports, want %d", len(reports), len(want)+1)
	}
	for _, r := range reports {
		got := r.Auth
		if r.Entry.Name == "broken" {
			if got.State != gateway.AuthUnreadable || got.Err == nil {
				t.Errorf("broken: auth = %+v, want unreadable with an error", got)
			}
			continue
		}
		w := want[r.Entry.Name]
		if got.State != w.State || !got.Expiry.Equal(w.Expiry) || got.Expired != w.Expired ||
			got.Renewable != w.Renewable || got.Reason != w.Reason || got.Remedy != w.Remedy || got.Err != nil {
			t.Errorf("%s: auth = %+v, want %+v", r.Entry.Name, got, w)
		}
	}
}

func TestReportsSayTheCredentialsOfAnOAuthUpstreamCantBeReadWithoutAStore(t *testing.T) {
	t.Parallel()
	c := openConfig(t, map[string]any{
		"oauth": map[string]any{"url": "https://oauth.invalid/mcp"},
		"local": map[string]any{"command": "fake-upstream"},
	})

	reports := gateway.Reports(t.Context(), c.Entries, nil, nil)

	// Config order is sorted by name: local, then oauth.
	if got := reports[1].Auth; got.State != gateway.AuthUnreadable || !errors.Is(got.Err, credentials.ErrNoHome) {
		t.Errorf("oauth: auth = %+v, want unreadable with %v", got, credentials.ErrNoHome)
	}
	if got := reports[0].Auth; got.State != gateway.AuthNotOAuth {
		t.Errorf("local: auth = %+v, want not an OAuth Upstream", got)
	}
}

func TestReportsWithACheckSayHowEachEntryTheGatewayStartsFares(t *testing.T) {
	t.Parallel()
	oauth := mcptest.StartOAuth(t)
	c := openConfig(t, map[string]any{
		"plain":   map[string]any{"url": mcptest.StartHTTP(t, "").URL},
		"oauth":   map[string]any{"url": oauth.URL},
		"dead":    map[string]any{"url": oauth.URL},
		"refused": map[string]any{"url": mcptest.RefusingURL()},
		"off":     map[string]any{"url": mcptest.RefusingURL(), "disabled": true},
		"unset":   map[string]any{"url": "https://unset.invalid/mcp?key=${SPRUT_TEST_UNSET}"},
	})
	store, state := newStoreIn(t)
	save(t, store, c, "dead", oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(-time.Hour)})
	check := &gateway.CheckOptions{
		Env:            []string{"XDG_STATE_HOME=" + state},
		StartupTimeout: 10 * time.Second,
		Version:        "test",
		Log:            slog.New(slog.DiscardHandler),
	}

	reports := gateway.Reports(t.Context(), c.Entries, store, check)

	if len(reports) != 6 {
		t.Fatalf("got %d reports, want 6", len(reports))
	}
	for _, r := range reports {
		h := r.Health
		switch r.Entry.Name {
		case "plain":
			if h == nil || h.State != gateway.HealthWorks || h.Tools != 2 || h.Err != nil || h.Remedy != "" {
				t.Errorf("plain: health = %+v, want it to work with 2 tools", h)
			}
		case "oauth", "dead":
			if want := "run: sprut upstream login -c " + c.Path + " " + r.Entry.Name; h == nil || h.State != gateway.HealthNeedsLogin || h.Err == nil || h.Remedy != want {
				t.Errorf("%s: health = %+v, want it to need a Login, with remedy %q", r.Entry.Name, h, want)
			}
		case "refused":
			if h == nil || h.State != gateway.HealthFailed || h.Err == nil || h.Remedy != "" {
				t.Errorf("refused: health = %+v, want it failed with an error and no remedy", h)
			}
		default:
			if h != nil {
				t.Errorf("%s: health = %+v, want none: the Gateway doesn't start it", r.Entry.Name, h)
			}
		}
	}
	if gateway.AllWork(reports) {
		t.Error("AllWork = true, want false: two checked entries don't work")
	}
}

func TestAllWorkHoldsWhenEveryCheckedEntryWorks(t *testing.T) {
	t.Parallel()
	works := &gateway.Health{State: gateway.HealthWorks}
	for _, tc := range []struct {
		name    string
		reports []gateway.Report
		want    bool
	}{
		{"none", nil, true},
		{"unchecked", []gateway.Report{{}}, true},
		{"working", []gateway.Report{{Health: works}, {}}, true},
		{"needs a Login", []gateway.Report{{Health: works}, {Health: &gateway.Health{State: gateway.HealthNeedsLogin}}}, false},
		{"failed", []gateway.Report{{Health: &gateway.Health{State: gateway.HealthFailed}}}, false},
	} {
		if got := gateway.AllWork(tc.reports); got != tc.want {
			t.Errorf("%s: AllWork = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// openConfig writes a Config with servers as its mcpServers and opens it,
// with no variables set.
func openConfig(t *testing.T, servers map[string]any) *config.Config {
	t.Helper()
	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Open(path, config.LookupIn(nil))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newStore is an empty Credentials Store of a test's own.
func newStore(t *testing.T) *credentials.Store {
	t.Helper()
	s, _ := newStoreIn(t)
	return s
}

// newStoreIn is newStore, and also returns its $XDG_STATE_HOME.
func newStoreIn(t *testing.T) (*credentials.Store, string) {
	t.Helper()
	state := t.TempDir()
	s, err := credentials.NewStore(config.LookupIn([]string{"XDG_STATE_HOME=" + state}))
	if err != nil {
		t.Fatal(err)
	}
	return s, state
}

// save stores token as the Credentials of the Upstream name of c.
func save(t *testing.T, store *credentials.Store, c *config.Config, name string, token oauth2.Token) {
	t.Helper()
	u, err := c.Upstream(name)
	if err != nil {
		t.Fatal(err)
	}
	creds := &credentials.Credentials{Token: token, Config: credentials.BindingOf(u)}
	if err := store.Save(t.Context(), name, creds); err != nil {
		t.Fatal(err)
	}
}
