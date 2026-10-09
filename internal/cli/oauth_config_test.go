package cli_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAuthLoginWithAPreRegisteredClientAndServeRenewingWithTheSecretFromTheConfig(t *testing.T) {
	t.Parallel()
	u := startOAuthUpstream(t)
	config := writeConfig(t, map[string]any{"fake": map[string]any{
		"url":   u.URL,
		"oauth": map[string]any{"clientId": fakeClientID, "clientSecret": "${SECRET}"},
	}})
	state := t.TempDir()
	creds := &loggedIn{oauthUpstream: u, creds: filepath.Join(state, "sprut", "credentials", "fake.json")}

	env := []string{"XDG_STATE_HOME=" + state, "SECRET=" + fakeClientSecret}
	mustLogin(t, u, env, config)
	if n := u.Registrations(); n != 0 {
		t.Errorf("client registrations = %d, want 0", n)
	}
	wantNoSecrets(t, "Credentials file", creds.readCreds(t), fakeClientSecret)

	// The secret is rotated in the Config, and the Credentials keep working.
	const rotated = "rotated-client-secret-0b7a"
	u.AcceptClientSecret(rotated)
	u.RevokeAccessTokens()
	env = []string{"XDG_STATE_HOME=" + state, "SECRET=" + rotated}
	g := startGateway(t, config, env)
	g.wantEcho(t)
	g.closeAgent(t)

	refreshes := u.Refreshes()
	if len(refreshes) != 1 || refreshes[0].Get("client_secret") != rotated {
		t.Errorf("refreshes = %v, want one with the rotated secret", refreshes)
	}
	wantNoSecrets(t, "Credentials file", creds.readCreds(t), fakeClientSecret, rotated)
}

func TestAuthLoginRequestsScopes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		challenge string   // the scope of the 401
		supported []string // the Protected Resource Metadata's scopes_supported
		oauth     []string // oauth.scopes in the Config
		offline   bool     // whether the Authorization Server advertises offline_access
		want      string   // "" for no scope parameter
	}{
		{name: "the challenge's", challenge: "read write", supported: []string{"other"}, want: "read write"},
		{name: "else scopes_supported", supported: []string{"read", "write"}, want: "read write"},
		{name: "else none"},
		{name: "oauth.scopes over the challenge's", challenge: "read", oauth: []string{"admin", "write"}, want: "admin write"},
		{name: "empty oauth.scopes for none", challenge: "read", oauth: []string{}},
		{name: "offline_access when advertised", challenge: "read", offline: true, want: "read offline_access"},
		{
			name: "offline_access with oauth.scopes", challenge: "read", oauth: []string{"write"}, offline: true,
			want: "write offline_access",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := startOAuthUpstream(t)
			u.OfferScopes(tt.challenge, tt.supported)
			if tt.offline {
				u.AdvertiseOfflineAccess()
			}
			var oauth map[string]any
			if tt.oauth != nil {
				oauth = map[string]any{"scopes": tt.oauth}
			}
			config := writeConfig(t, map[string]any{"fake": upstreamEntry(u.URL, oauth)})

			mustLogin(t, u, []string{"XDG_STATE_HOME=" + t.TempDir()}, config)
			if got, has := u.RequestedScope(); has != (tt.want != "") || !sameScopes(got, tt.want) {
				t.Errorf("requested scope = %q (present %v), want %q in any order", got, has, tt.want)
			}
		})
	}
}

// sameScopes reports whether the scope parameters a and b name the same
// scopes, in any order.
func sameScopes(a, b string) bool {
	return slices.Equal(slices.Sorted(slices.Values(strings.Fields(a))), slices.Sorted(slices.Values(strings.Fields(b))))
}

func TestServeReportsInsufficientScopeWithALoginHintAndTheNextLoginKeepsTheGrantedScopes(t *testing.T) {
	t.Parallel()
	u := startOAuthUpstream(t)
	u.OfferScopes("read", nil)
	l := logInTo(t, u, 3600)
	g := startGateway(t, l.config, l.env)
	g.wantEcho(t)

	// The Upstream now wants a scope it didn't grant, and says so in its 401s too.
	u.RequireScope("write")
	u.OfferScopes("write", nil)
	wantLoginHint(t, l.config, g.echo(context.Background(), t))
	g.closeAgent(t)

	mustLogin(t, u, l.env, l.config)
	if got, _ := u.RequestedScope(); !sameScopes(got, "read write") {
		t.Errorf("step-up Login requested scope = %q, want read and write", got)
	}
	g = startGateway(t, l.config, l.env)
	g.wantEcho(t)
	g.closeAgent(t)
}

func TestServeUsesCredentialsOnlyForWhatTheyWereIssuedFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		login     map[string]any // the oauth of the Config at the Login, if any
		serve     map[string]any // and at serve
		localhost bool           // whether serve names the Upstream's host localhost, not 127.0.0.1
		needLogin bool
	}{
		{name: "changed url", localhost: true, needLogin: true},
		{name: "added oauth.clientId", serve: map[string]any{"clientId": fakeClientID}, needLogin: true},
		{
			name:      "changed oauth.clientId",
			login:     map[string]any{"clientId": fakeClientID, "clientSecret": fakeClientSecret},
			serve:     map[string]any{"clientId": "other-client", "clientSecret": fakeClientSecret},
			needLogin: true,
		},
		{name: "added oauth.scopes", serve: map[string]any{"scopes": []string{fakeScope}}, needLogin: true},
		{
			name:      "changed oauth.scopes",
			login:     map[string]any{"scopes": []string{fakeScope}},
			serve:     map[string]any{"scopes": []string{fakeScope, "admin"}},
			needLogin: true,
		},
		{
			name:  "reordered oauth.scopes",
			login: map[string]any{"scopes": []string{fakeScope, "admin"}},
			serve: map[string]any{"scopes": []string{"admin", fakeScope}},
		},
		{
			name:  "changed oauth.clientSecret",
			login: map[string]any{"clientId": fakeClientID, "clientSecret": fakeClientSecret},
			serve: map[string]any{"clientId": fakeClientID, "clientSecret": "rotated-client-secret-0b7a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u := startOAuthUpstream(t)
			env := []string{"XDG_STATE_HOME=" + t.TempDir()}
			config := writeConfig(t, map[string]any{"fake": upstreamEntry(u.URL, tt.login)})
			mustLogin(t, u, env, config)

			link := u.URL
			if tt.localhost {
				link = strings.Replace(link, "127.0.0.1", "localhost", 1)
			}
			before := len(u.MCPAuth())
			serveConfig := writeConfig(t, map[string]any{"fake": upstreamEntry(link, tt.serve)})
			g := startGateway(t, serveConfig, env)
			if !tt.needLogin {
				g.wantEcho(t)
				g.closeAgent(t)
				return
			}
			g.wantTools(t)
			g.wantLogLine(t, "WARN", `msg="upstream failed; skipped"`, "upstream=fake", `hint="`+loginHint(serveConfig, "fake")+`"`)
			g.closeAgent(t)
			for i, got := range u.MCPAuth()[before:] {
				if got != "" {
					t.Errorf("request %d from serve: Authorization = %q, want none", i, got)
				}
			}
		})
	}
}

// upstreamEntry is the Config entry of an HTTP Upstream at link, with
// oauth if it is not nil.
func upstreamEntry(link string, oauth map[string]any) map[string]any {
	e := map[string]any{"url": link}
	if oauth != nil {
		e["oauth"] = oauth
	}
	return e
}
