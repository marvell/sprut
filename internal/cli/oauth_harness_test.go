package cli_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/cli"
)

// The secrets the fake Authorization Server hands out, distinctive enough
// that finding one in sprut's output can only mean a leak.
const (
	fakeClientID     = "fake-client"
	fakeClientSecret = "fake-client-secret-c3f0"
	fakeCode         = "fake-auth-code-55e2"
	fakeAccessToken  = "fake-access-token-7d1e"
	fakeRefreshToken = "fake-refresh-token-91ab"
	fakeScope        = "mcp:tools"
)

// fakeSecrets are every secret of the fake Authorization Server.
var fakeSecrets = []string{fakeClientSecret, fakeCode, fakeAccessToken, fakeRefreshToken}

// oauthUpstream is a fake OAuth Upstream and its Authorization Server on one
// httptest server: Protected Resource Metadata, Authorization Server
// metadata, Dynamic Client Registration, an authorization endpoint that
// redirects straight to the callback, a token endpoint, and an MCP endpoint
// at /mcp that answers 401 to any request without the access token.
type oauthUpstream struct {
	Base string // the server's origin, which is also the issuer
	URL  string // the MCP endpoint

	mu         sync.Mutex
	challenge  string     // PKCE code challenge of the last authorization request
	grantTypes []string   // of the last client registration
	appType    string     // application_type of the last client registration
	redirect   url.Values // added to the callback in place of the code
	mcpAuth    []string   // the Authorization header of each request to /mcp
}

func startOAuthUpstream(t *testing.T) *oauthUpstream {
	t.Helper()
	u := &oauthUpstream{}
	server := newFakeServer("")
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"resource":              u.URL,
			"authorization_servers": []string{u.Base},
			"scopes_supported":      []string{fakeScope},
		})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                u.Base,
			"authorization_endpoint":                u.Base + "/authorize",
			"token_endpoint":                        u.Base + "/token",
			"registration_endpoint":                 u.Base + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_post"},
		})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var meta map[string]any
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client_metadata"})
			return
		}
		var grantTypes []string
		for _, g := range meta["grant_types"].([]any) {
			grantTypes = append(grantTypes, g.(string))
		}
		u.mu.Lock()
		u.grantTypes = grantTypes
		u.appType, _ = meta["application_type"].(string)
		u.mu.Unlock()
		meta["client_id"] = fakeClientID
		meta["client_secret"] = fakeClientSecret
		meta["token_endpoint_auth_method"] = "client_secret_post"
		writeJSON(w, http.StatusCreated, meta)
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != fakeClientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "bad authorization request", http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.challenge = q.Get("code_challenge")
		u.mu.Unlock()
		callback, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "bad redirect_uri", http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		cq := maps.Clone(u.redirect)
		u.mu.Unlock()
		if cq == nil {
			cq = url.Values{"code": {fakeCode}}
		}
		cq.Set("state", q.Get("state"))
		callback.RawQuery = cq.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
			return
		}
		u.mu.Lock()
		challenge := u.challenge
		u.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != fakeCode ||
			r.PostForm.Get("client_secret") != fakeClientSecret ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  fakeAccessToken,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": fakeRefreshToken,
			"scope":         fakeScope,
		})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		u.mu.Lock()
		u.mcpAuth = append(u.mcpAuth, got)
		u.mu.Unlock()
		if got != "Bearer "+fakeAccessToken {
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+u.Base+`/.well-known/oauth-protected-resource/mcp", scope="`+fakeScope+`"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	u.Base = ts.URL
	u.URL = ts.URL + "/mcp"
	return u
}

// MCPAuth returns the Authorization header of each request to the MCP
// endpoint so far.
func (u *oauthUpstream) MCPAuth() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.mcpAuth)
}

// GrantTypes returns the grant types of the last client registration.
func (u *oauthUpstream) GrantTypes() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.grantTypes)
}

// RedirectWith makes the authorization endpoint redirect to the callback
// with params and the state, in place of the code.
func (u *oauthUpstream) RedirectWith(params url.Values) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.redirect = params
}

// AppType returns the application_type of the last client registration.
func (u *oauthUpstream) AppType() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.appType
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// authURL finds the authorization URL that sprut printed for the
// Authorization Server at base.
func authURL(base string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(base+"/authorize?") + `\S+`)
}

// login runs `sprut auth login` with args (after "auth login") and env, and
// plays the user: it waits for the authorization URL on stderr and opens it,
// following the Authorization Server's redirect to sprut's callback. It
// returns sprut's exit code and stderr.
func login(t *testing.T, u *oauthUpstream, env []string, args ...string) (code int, stderr string) {
	t.Helper()
	l := startLogin(t, 30*time.Second, env, args...)
	if link := l.authURL(t, u); link != nil {
		if resp := l.do(t, http.MethodGet, link.String()); resp.StatusCode != http.StatusOK {
			t.Errorf("callback answered %s", resp.Status)
		}
	}
	return l.wait(t)
}

// loginRun is a `sprut auth login` running in the background.
type loginRun struct {
	ctx      context.Context
	deadline time.Time
	stderr   *syncBuffer
	exit     chan int
}

// startLogin starts `sprut auth login` with args (after "auth login") and
// env, which must exit within timeout.
func startLogin(t *testing.T, timeout time.Duration, env []string, args ...string) *loginRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	l := &loginRun{ctx: ctx, deadline: time.Now().Add(timeout), stderr: &syncBuffer{}, exit: make(chan int, 1)}
	go func() {
		l.exit <- cli.Run(ctx, append([]string{"sprut", "auth", "login"}, args...), env,
			strings.NewReader(""), io.Discard, l.stderr)
	}()
	return l
}

// authURL waits for the authorization URL of the Authorization Server of u
// on stderr, and returns it, or nil if sprut exits first.
func (l *loginRun) authURL(t *testing.T, u *oauthUpstream) *url.URL {
	t.Helper()
	var link string
	pattern := authURL(u.Base)
	found := eventually(func() bool {
		if l.exited() {
			return true
		}
		link = pattern.FindString(l.stderr.String())
		return link != ""
	})
	if !found {
		t.Fatalf("no authorization URL on stderr:\n%s", l.stderr)
	}
	if link == "" {
		return nil
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parsing the authorization URL: %v", err)
	}
	return parsed
}

// mustAuthURL is authURL, failing the test if sprut exits first.
func (l *loginRun) mustAuthURL(t *testing.T, u *oauthUpstream) *url.URL {
	t.Helper()
	link := l.authURL(t, u)
	if link == nil {
		code, stderr := l.wait(t)
		t.Fatalf("auth login exited %d before printing the authorization URL\nstderr:\n%s", code, stderr)
	}
	return link
}

// exited reports whether sprut has exited, without waiting.
func (l *loginRun) exited() bool {
	select {
	case code := <-l.exit:
		l.exit <- code
		return true
	default:
		return false
	}
}

// do sends a request with method to link, following redirects, and returns
// the response, its body already closed.
func (l *loginRun) do(t *testing.T, method, link string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(l.ctx, method, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, link, err)
	}
	_ = resp.Body.Close()
	return resp
}

// wait returns sprut's exit code and stderr once it exits.
func (l *loginRun) wait(t *testing.T) (code int, stderr string) {
	t.Helper()
	select {
	case code = <-l.exit:
	case <-time.After(time.Until(l.deadline) + 10*time.Second):
		t.Fatalf("sprut auth login did not exit\nstderr:\n%s", l.stderr)
	}
	return code, l.stderr.String()
}
