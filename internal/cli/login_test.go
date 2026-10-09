package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/marvell/sprut/internal/cli"
	"github.com/marvell/sprut/internal/mcptest"
)

// loginAs logs in to u as the Upstream name of config, with its
// Credentials under state, and returns them.
func loginAs(t *testing.T, u *mcptest.OAuthUpstream, state, config, name string) *loggedIn {
	t.Helper()
	l := &loggedIn{
		OAuthUpstream: u,
		config:        config,
		env:           []string{"XDG_STATE_HOME=" + state},
		creds:         filepath.Join(state, "sprut", "credentials", name+".json"),
	}
	code, _ := login(t, u, l.env, name, "--no-browser", "-c", config)
	if code != 0 {
		t.Fatalf("auth login %s exit code = %d, want 0", name, code)
	}
	return l
}

// makeStale makes the access token of l's Credentials expired, though they
// can still be renewed.
func (l *loggedIn) makeStale(t *testing.T) {
	t.Helper()
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	l.rewriteCreds(t, func(f map[string]any) { f["expiry"] = past })
}

// makeDead makes l's Credentials expired, without a refresh token.
func (l *loggedIn) makeDead(t *testing.T) {
	t.Helper()
	l.makeStale(t)
	l.rewriteCreds(t, func(f map[string]any) { delete(f, "refresh_token") })
}

// playLogins waits for l to exit, opening each authorization URL of the
// Authorization Server of u that it prints, as the user would. It returns
// sprut's exit code and stderr.
func (l *loginRun) playLogins(t *testing.T, u *mcptest.OAuthUpstream) (code int, stderr string) {
	t.Helper()
	pattern := authURLPattern(u.Base)
	for opened := 0; ; opened++ {
		var links []string
		eventually(func() bool {
			links = pattern.FindAllString(l.stderr.String(), -1)
			return len(links) > opened || l.exited()
		})
		if len(links) <= opened {
			return l.wait(t)
		}
		l.do(t, http.MethodGet, links[opened])
	}
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

// authURLPattern finds the authorization URL that sprut printed for the
// Authorization Server at base.
func authURLPattern(base string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(base+"/authorize?") + `\S+`)
}

// login runs `sprut auth login` with args (after "auth login") and env, and
// plays the user: it waits for the authorization URL on stderr and opens it,
// following the Authorization Server's redirect to sprut's callback. It
// returns sprut's exit code and stderr.
func login(t *testing.T, u *mcptest.OAuthUpstream, env []string, args ...string) (code int, stderr string) {
	t.Helper()
	l := startLogin(t, 30*time.Second, env, args...)
	if link := l.authURL(t, u); link != nil {
		if resp := l.do(t, http.MethodGet, link.String()); resp.StatusCode != http.StatusOK {
			t.Errorf("callback answered %s", resp.Status)
		}
	}
	return l.wait(t)
}

// mustLogin logs in to u, the Upstream "fake" in config, with args added,
// failing the test unless the Login succeeds. It returns sprut's stderr.
func mustLogin(t *testing.T, u *mcptest.OAuthUpstream, env []string, config string, args ...string) string {
	t.Helper()
	code, stderr := login(t, u, env, append([]string{"fake", "--no-browser", "-c", config}, args...)...)
	if code != 0 {
		t.Fatalf("auth login exit code = %d, want 0", code)
	}
	return stderr
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
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	t.Cleanup(cancel)
	l := &loginRun{ctx: ctx, deadline: time.Now().Add(timeout), stderr: &syncBuffer{}, exit: make(chan int, 1)}
	go func() {
		l.exit <- cli.Run(ctx, append([]string{"sprut", "auth", "login"}, args...), env,
			strings.NewReader(""), io.Discard, io.MultiWriter(l.stderr, testLog(t, "sprut auth login")))
	}()
	return l
}

// authURL waits for the authorization URL of the Authorization Server of u
// on stderr, and returns it, or nil if sprut exits first.
func (l *loginRun) authURL(t *testing.T, u *mcptest.OAuthUpstream) *url.URL {
	t.Helper()
	var link string
	pattern := authURLPattern(u.Base)
	found := eventually(func() bool {
		if l.exited() {
			return true
		}
		link = pattern.FindString(l.stderr.String())
		return link != ""
	})
	if !found {
		t.Fatalf("no authorization URL on stderr")
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
func (l *loginRun) mustAuthURL(t *testing.T, u *mcptest.OAuthUpstream) *url.URL {
	t.Helper()
	link := l.authURL(t, u)
	if link == nil {
		code, _ := l.wait(t)
		t.Fatalf("auth login exited %d before printing the authorization URL", code)
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
		t.Fatalf("sprut auth login did not exit")
	}
	return code, l.stderr.String()
}

// loggedIn is an OAuth Upstream, "fake", that `sprut auth login` has stored
// Credentials for.
type loggedIn struct {
	*mcptest.OAuthUpstream
	config string
	env    []string
	creds  string // the path of the Credentials file
}

// logIn starts a fake OAuth Upstream whose access tokens live for expiresIn
// seconds, and logs in to it.
func logIn(t *testing.T, expiresIn int) *loggedIn {
	t.Helper()
	return logInTo(t, mcptest.StartOAuth(t), expiresIn)
}

// logInTo logs in to u, whose access tokens live for expiresIn seconds.
func logInTo(t *testing.T, u *mcptest.OAuthUpstream, expiresIn int) *loggedIn {
	t.Helper()
	u.ExpireIn(expiresIn)
	return loginAs(t, u, t.TempDir(), writeConfig(t, map[string]any{"fake": map[string]any{"url": u.URL}}), "fake")
}

// readCreds returns the content of the Credentials file.
func (l *loggedIn) readCreds(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(l.creds)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// wantCredsUnchanged checks that the Credentials file still holds before.
func (l *loggedIn) wantCredsUnchanged(t *testing.T, before string) {
	t.Helper()
	if got := l.readCreds(t); got != before {
		t.Errorf("Credentials changed:\nbefore: %s\nafter:  %s", before, got)
	}
}

// leakySecrets are mcptest.Secrets and what the fake Authorization Server says
// around them in an error_description.
var leakySecrets = append(slices.Clone(mcptest.Secrets), "of client")

// loginHint is the remedy that sprut gives for the Upstream name in the
// Config at path, which tests always name with -c.
func loginHint(path, name string) string {
	return "run: sprut auth login -c " + path + " " + name
}

// wantLoginHint checks that res is an isError result with the Login hint of
// fake in the Config at path.
func wantLoginHint(t *testing.T, path string, res *mcp.CallToolResult) {
	t.Helper()
	text := resultText(t, res)
	if !res.IsError || !strings.Contains(text, loginHint(path, "fake")) {
		t.Errorf("fake__echo = %q (isError %v), want an isError result with the login hint", text, res.IsError)
	}
	wantNoSecrets(t, "tool result", text, leakySecrets...)
}

// wantOneRefreshPerRotation checks that no refresh token was sent to the
// Authorization Server twice: a second refresh with one would mean that
// two processes renewed the same Credentials.
func wantOneRefreshPerRotation(t *testing.T, u *mcptest.OAuthUpstream) {
	t.Helper()
	seen := map[string]bool{}
	for _, form := range u.Refreshes() {
		token := form.Get("refresh_token")
		if seen[token] {
			t.Errorf("refresh token %q was sent more than once", token)
		}
		seen[token] = true
	}
}

// rewriteCreds replaces the Credentials file with a new one, as a write by
// sprut would, after edit changes its JSON fields. It takes no lock.
func (l *loggedIn) rewriteCreds(t *testing.T, edit func(fields map[string]any)) {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(l.readCreds(t)), &fields); err != nil {
		t.Fatal(err)
	}
	edit(fields)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	tmp := l.creds + ".test"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, l.creds); err != nil {
		t.Fatal(err)
	}
}

// lockCreds takes the lock that sprut renews the Credentials under, as
// another sprut process would, and returns what releases it.
func (l *loggedIn) lockCreds(t *testing.T) (unlock func()) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(filepath.Dir(l.creds), "fake.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	unlock = func() { _ = f.Close() }
	t.Cleanup(unlock)
	return unlock
}

// startEcho calls fake__echo through g in the background, once the Upstream
// rejects the current access token, and returns where its result arrives
// once the call has been rejected.
func (l *loggedIn) startEcho(t *testing.T, g *gateway) <-chan *callOutcome {
	t.Helper()
	rejected := l.Rejected()
	done := make(chan *callOutcome, 1)
	go func() {
		res, err := g.callEcho(t.Context())
		done <- &callOutcome{res, err}
	}()
	if !eventually(func() bool { return l.Rejected() > rejected }) {
		t.Fatalf("the call was not rejected")
	}
	return done
}

type callOutcome struct {
	res *mcp.CallToolResult
	err error
}

// wantNoCreds checks that the Credentials file does not exist.
func wantNoCreds(t *testing.T, l *loggedIn) {
	t.Helper()
	if _, err := os.Stat(l.creds); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the Credentials file exists (%v), want it gone", err)
	}
}
