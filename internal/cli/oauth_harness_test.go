package cli_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/marvell/sprut/internal/cli"
	"github.com/marvell/sprut/internal/mcptest"
)

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
		t.Fatalf("auth login exit code = %d, want 0\nstderr:\n%s", code, stderr)
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
func (l *loginRun) mustAuthURL(t *testing.T, u *mcptest.OAuthUpstream) *url.URL {
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
