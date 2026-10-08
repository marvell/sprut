// Package config turns a Config file into the Upstream definitions it declares.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Transport is how the Gateway talks to an Upstream.
type Transport string

const (
	Stdio Transport = "stdio"
	HTTP  Transport = "http"
)

// Upstream is one resolved Upstream definition from the Config.
type Upstream struct {
	Name      string
	Transport Transport

	// stdio
	Command string
	Args    []string
	Env     map[string]string // layered on top of the inherited environment

	// HTTP
	URL     string
	Headers map[string]string
	OAuth   *OAuth // nil when the Config has no "oauth"
}

// OAuth is how sprut logs in to an OAuth Upstream, where the Config says.
type OAuth struct {
	// A pre-registered client, in place of Dynamic Client Registration.
	ClientID     string
	ClientSecret string
	// The scopes to request, in place of those the Upstream asks for; nil
	// when the Config has none.
	Scopes []string
}

// Warning is a problem in the Config that does not stop the Gateway.
type Warning struct {
	Upstream string
	Message  string
}

// LookupEnv looks up an environment variable, like os.LookupEnv.
type LookupEnv func(string) (string, bool)

// LookupIn returns a lookup over env, which is in os.Environ form. As with
// a real environment, the last entry for a key wins.
func LookupIn(env []string) LookupEnv {
	vars := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

// Path resolves which Config file to read: flag (from -c/--config), then
// $SPRUT_CONFIG, then $XDG_CONFIG_HOME/sprut/config.json, then
// ~/.config/sprut/config.json.
func Path(flag string, lookupEnv LookupEnv) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if p, _ := lookupEnv("SPRUT_CONFIG"); p != "" {
		return p, nil
	}
	if dir, ok := XDGDir(lookupEnv, "XDG_CONFIG_HOME", ".config"); ok {
		return filepath.Join(dir, "sprut", "config.json"), nil
	}
	return "", errors.New("cannot locate the config: HOME is not set; use -c PATH or SPRUT_CONFIG")
}

// XDGDir returns the base directory that the XDG variable names, or the
// fallback under $HOME that the XDG spec gives for it. It reports false if
// neither is set.
func XDGDir(lookupEnv LookupEnv, variable, fallback string) (string, bool) {
	// The XDG spec says relative paths are invalid and must be ignored.
	if dir, _ := lookupEnv(variable); filepath.IsAbs(dir) {
		return dir, true
	}
	if home, _ := lookupEnv("HOME"); home != "" {
		return filepath.Join(home, fallback), true
	}
	return "", false
}

// validName keeps Namespaced tool names unambiguous: an Upstream name never
// contains the "__" separator.
var validName = regexp.MustCompile(`^[a-z0-9-]+$`)

// Parse parses Config bytes into the Upstreams to start, in Config order,
// plus warnings about the Config. lookupEnv resolves ${VAR} references.
func Parse(data []byte, lookupEnv LookupEnv) ([]Upstream, []Warning, error) {
	var f struct {
		MCPServers servers `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, jsonError(data, err)
	}

	var (
		upstreams []Upstream
		warnings  []Warning
		errs      []error
	)
	for _, name := range f.MCPServers.names {
		u, msgs, err := parseUpstream(name, f.MCPServers.entries[name], lookupEnv)
		if err != nil {
			errs = append(errs, fmt.Errorf("upstream %q: %w", name, err))
			continue
		}
		for _, msg := range msgs {
			warnings = append(warnings, Warning{Upstream: name, Message: msg})
		}
		if u != nil {
			upstreams = append(upstreams, *u)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}
	return upstreams, warnings, nil
}

// servers are the entries of mcpServers, keeping the order of their names.
// As when decoding into a map, a name given twice keeps its last entry; it
// keeps its first place. Unlike decoding into a map, which merges, a repeated
// mcpServers key replaces the servers given before it.
type servers struct {
	names   []string
	entries map[string]json.RawMessage
}

func (s *servers) UnmarshalJSON(data []byte) error {
	*s = servers{}
	if string(data) == "null" {
		return nil
	}
	// json.Unmarshal has checked the syntax before calling this, so only
	// the shape can be wrong.
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, _ := dec.Token(); tok != json.Delim('{') {
		return errors.New("mcpServers must be a JSON object")
	}
	s.entries = map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		name := tok.(string)
		var entry json.RawMessage
		if err := dec.Decode(&entry); err != nil {
			return err
		}
		if _, ok := s.entries[name]; !ok {
			s.names = append(s.names, name)
		}
		s.entries[name] = entry
	}
	return nil
}

// parseUpstream parses one Config entry. It returns a nil Upstream, and
// warnings saying why, when the entry is valid but must not be started.
func parseUpstream(name string, raw json.RawMessage, lookupEnv LookupEnv) (*Upstream, []string, error) {
	if !validName.MatchString(name) {
		return nil, nil, fmt.Errorf("invalid name, must match %s", validName)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, nil, errors.New("must be a JSON object")
	}

	u := Upstream{Name: name}
	var (
		typ      string
		disabled bool
		oauth    map[string]json.RawMessage
	)
	// known are the fields of an entry, by the transport they apply to ("" for
	// every entry).
	known := map[string]struct {
		transport Transport
		dst       any
		want      string
	}{
		"type":     {"", &typ, "a string"},
		"disabled": {"", &disabled, "a boolean"},
		"command":  {"", &u.Command, "a string"},
		"url":      {"", &u.URL, "a string"},
		"args":     {Stdio, &u.Args, "an array of strings"},
		"env":      {Stdio, &u.Env, "an object of strings"},
		"headers":  {HTTP, &u.Headers, "an object of strings"},
		"oauth":    {HTTP, &oauth, "an object"},
	}
	decode := func(name string) error {
		f := known[name]
		return decodeField(fields, name, f.dst, f.want)
	}
	for _, name := range slices.Sorted(maps.Keys(known)) {
		if known[name].transport == "" {
			if err := decode(name); err != nil {
				return nil, nil, err
			}
		}
	}
	// A disabled entry is not checked further, so that disabling is a way to
	// set aside an entry that is broken or whose variables are unset.
	if disabled {
		return nil, nil, nil
	}

	u.Transport = Transport(typ)
	switch u.Transport {
	case "":
		t, err := inferTransport(u.Command, u.URL)
		if err != nil {
			return nil, nil, err
		}
		u.Transport = t
	case Stdio, HTTP:
		if err := checkTarget(u); err != nil {
			return nil, nil, err
		}
	case "sse":
		return nil, []string{"SSE transport is not supported; upstream skipped"}, nil
	default:
		return nil, nil, fmt.Errorf(`unknown type %q, want "stdio", "http" or "sse"`, typ)
	}

	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		f, ok := known[name]
		switch {
		case !ok:
			warnings = append(warnings, fmt.Sprintf("unknown field %q ignored", name))
		case f.transport == "":
		case f.transport != u.Transport:
			warnings = append(warnings, fmt.Sprintf("field %q does not apply to %s upstreams; ignored", name, u.Transport))
		default:
			if err := decode(name); err != nil {
				return nil, nil, err
			}
		}
	}
	if oauth != nil {
		var (
			msgs []string
			err  error
		)
		if u.OAuth, msgs, err = parseOAuth(oauth); err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, msgs...)
	}

	if missing := interpolate(&u, lookupEnv); len(missing) > 0 {
		return nil, append(warnings, unsetWarning(missing)), nil
	}
	// The target was checked before interpolation, but a variable set to empty
	// can still empty it. "command" is not interpolated, so only "url" can.
	if u.Transport == HTTP && u.URL == "" {
		return nil, append(warnings, `"url" is empty after interpolation; upstream skipped`), nil
	}
	return &u, warnings, nil
}

// parseOAuth parses the fields of "oauth", with warnings about the ones
// sprut doesn't know.
func parseOAuth(fields map[string]json.RawMessage) (*OAuth, []string, error) {
	o := &OAuth{}
	known := map[string]struct {
		dst  any
		want string
	}{
		"clientId":     {&o.ClientID, "a string"},
		"clientSecret": {&o.ClientSecret, "a string"},
		"scopes":       {&o.Scopes, "an array of strings"},
	}
	var warnings []string
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		f, ok := known[name]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("unknown field %q ignored", "oauth."+name))
			continue
		}
		if err := decodeField(fields, name, f.dst, f.want); err != nil {
			return nil, nil, fmt.Errorf("oauth: %w", err)
		}
	}
	return o, warnings, nil
}

// decodeField decodes the field name of fields, if there is one, into dst,
// or says that it must be want.
func decodeField(fields map[string]json.RawMessage, name string, dst any, want string) error {
	if v, ok := fields[name]; ok && (hasNull(v) || json.Unmarshal(v, dst) != nil) {
		return fmt.Errorf("%q must be %s", name, want)
	}
	return nil
}

// hasNull reports whether v is null or holds a null as an array element or
// object value. json.Unmarshal takes null as the zero value, so a null would
// silently turn into a different Config.
func hasNull(v json.RawMessage) bool {
	var x any
	if json.Unmarshal(v, &x) != nil {
		return false
	}
	switch x := x.(type) {
	case nil:
		return true
	case []any:
		return slices.Contains(x, nil)
	case map[string]any:
		for _, e := range x {
			if e == nil {
				return true
			}
		}
	}
	return false
}

// inferTransport picks the transport of an entry without "type" from
// whichever of "command" and "url" it sets.
func inferTransport(command, url string) (Transport, error) {
	switch {
	case command != "" && url != "":
		return "", errors.New(`has both "command" and "url"; set only one`)
	case command != "":
		return Stdio, nil
	case url != "":
		return HTTP, nil
	default:
		return "", errors.New(`needs "command" (stdio) or "url" (http)`)
	}
}

// checkTarget checks that u, whose "type" is set, sets the target its transport needs ("command"
// for stdio, "url" for HTTP) and not the other one.
func checkTarget(u Upstream) error {
	set := map[string]bool{"command": u.Command != "", "url": u.URL != ""}
	need, other := "command", "url"
	if u.Transport == HTTP {
		need, other = other, need
	}
	if !set[need] || set[other] {
		return fmt.Errorf("type %q needs %q and no %q", u.Transport, need, other)
	}
	return nil
}

// unsetWarning says which variables are unset, so the Upstream is skipped.
func unsetWarning(missing []string) string {
	noun := "variable"
	if len(missing) > 1 {
		noun = "variables"
	}
	return fmt.Sprintf("unset %s %s; upstream skipped", noun, strings.Join(missing, ", "))
}

// varRef is a ${VAR} reference.
var varRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// interpolate replaces ${VAR} references in the fields that allow them (in
// env and headers, only the values; in oauth, all of it) and returns the names of unset variables, sorted.
func interpolate(u *Upstream, lookupEnv LookupEnv) []string {
	missing := map[string]bool{}
	expand := func(s string) string {
		return varRef.ReplaceAllStringFunc(s, func(ref string) string {
			name := ref[len("${") : len(ref)-len("}")]
			v, ok := lookupEnv(name)
			if !ok {
				missing[name] = true
			}
			return v
		})
	}
	for i, a := range u.Args {
		u.Args[i] = expand(a)
	}
	for k, v := range u.Env {
		u.Env[k] = expand(v)
	}
	u.URL = expand(u.URL)
	for k, v := range u.Headers {
		u.Headers[k] = expand(v)
	}
	if o := u.OAuth; o != nil {
		o.ClientID = expand(o.ClientID)
		o.ClientSecret = expand(o.ClientSecret)
		for i, s := range o.Scopes {
			o.Scopes[i] = expand(s)
		}
	}
	return slices.Sorted(maps.Keys(missing))
}

// jsonError adds the line and column of a syntax error, which encoding/json
// reports only as a byte offset.
func jsonError(data []byte, err error) error {
	var syntax *json.SyntaxError
	// Past the end there is no offending byte to point at.
	if !errors.As(err, &syntax) || syntax.Offset >= int64(len(data)) {
		return err
	}
	// Offset counts the offending byte too, so before ends with it.
	before := data[:syntax.Offset]
	line := bytes.Count(before, []byte("\n")) + 1
	col := len(before) - bytes.LastIndexByte(before, '\n') - 1
	return fmt.Errorf("line %d, column %d: %w", line, col, err)
}
