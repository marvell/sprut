package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// exchangeLog sends requests through next and logs each exchange at DEBUG:
// its method, URL and status, and as much of the bodies as carries no
// secret. Bodies are logged only of requests that send no secret and are
// not to the Upstream: discovery and Dynamic Client Registration, whose
// response has its secrets redacted. Of the rest, an error response gives
// only its OAuth error code, as errorBodies keeps it, and a challenge to
// authorize only its WWW-Authenticate.
type exchangeLog struct {
	next     http.RoundTripper
	log      *slog.Logger
	upstream *url.URL
}

// secretFields are the fields of a JSON body that exchangeLog redacts.
var secretFields = []string{"client_secret", "registration_access_token", "access_token", "refresh_token", "id_token"}

func (t exchangeLog) RoundTrip(req *http.Request) (*http.Response, error) {
	attrs := []any{"method", req.Method, "url", logURL(req)}
	toUpstream := bareURL(req.URL) == bareURL(t.upstream)
	// Discovery is a GET and registration a JSON POST; a token request is
	// a form, with the code, verifier or refresh token, and client secret.
	bodies := !toUpstream && (req.Method == http.MethodGet || isJSON(req.Header))
	if bodies && req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			data, _ := io.ReadAll(io.LimitReader(body, maxMetadataBytes))
			_ = body.Close()
			if len(data) > 0 {
				attrs = append(attrs, "request", redactJSON(data))
			}
		}
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		t.log.Debug("http exchange", append(attrs, "error", err)...)
		return resp, err
	}
	attrs = append(attrs, "status", resp.StatusCode)
	if loc := resp.Header.Get("Location"); loc != "" && resp.StatusCode/100 == 3 {
		attrs = append(attrs, "location", strings.SplitN(loc, "?", 2)[0])
	}
	if challenge := resp.Header.Get("WWW-Authenticate"); challenge != "" {
		attrs = append(attrs, "www_authenticate", challenge)
	}
	switch {
	case toUpstream:
	case resp.StatusCode >= 400:
		data, rerr := peek(resp, maxErrorBytes)
		if rerr != nil {
			return nil, rerr
		}
		if code := oauthErrorCode(bytes.NewReader(data)); code != "" {
			attrs = append(attrs, "oauth_error", code)
		}
	case bodies && resp.StatusCode/100 == 2:
		data, rerr := peek(resp, maxMetadataBytes)
		if rerr != nil {
			return nil, rerr
		}
		attrs = append(attrs, "response", redactJSON(data))
	}
	t.log.Debug("http exchange", attrs...)
	return resp, nil
}

// logURL is the URL of req without its userinfo or query, either of which
// may hold a secret.
func logURL(req *http.Request) string {
	s := bareURL(req.URL)
	if req.URL.RawQuery != "" {
		s += "?<redacted>"
	}
	return s
}

// bareURL is u without its userinfo, query or fragment.
func bareURL(u *url.URL) string {
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

func isJSON(h http.Header) bool {
	mt, _, _ := mime.ParseMediaType(h.Get("Content-Type"))
	return mt == "application/json"
}

// peek reads up to limit bytes of resp's body and puts them back in front
// of the rest.
func peek(resp *http.Response, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(data), resp.Body), resp.Body}
	return data, nil
}

// redactJSON returns the JSON object data with its secretFields redacted,
// or, if it is not a JSON object, only its length.
func redactJSON(data []byte) string {
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return "(not a JSON object, " + strconv.Itoa(len(data)) + " bytes)"
	}
	for _, f := range secretFields {
		if _, ok := obj[f]; ok {
			obj[f] = "<redacted>"
		}
	}
	var out strings.Builder
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // keeps <redacted> legible
	_ = enc.Encode(obj)
	return strings.TrimSuffix(out.String(), "\n")
}
