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
}

// Warning is a problem in the Config that does not stop the Gateway.
type Warning struct {
	Upstream string
	Message  string
}

// LookupEnv looks up an environment variable, like os.LookupEnv.
type LookupEnv func(string) (string, bool)

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
	// The XDG spec says relative paths are invalid and must be ignored.
	if dir, _ := lookupEnv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "sprut", "config.json"), nil
	}
	if home, _ := lookupEnv("HOME"); home != "" {
		return filepath.Join(home, ".config", "sprut", "config.json"), nil
	}
	return "", errors.New("cannot locate the config: HOME is not set; use -c PATH or SPRUT_CONFIG")
}

// validName keeps Namespaced tool names unambiguous: an Upstream name never
// contains the "__" separator.
var validName = regexp.MustCompile(`^[a-z0-9-]+$`)

// Parse parses Config bytes into the Upstreams to start, ordered by name,
// plus warnings about the Config. lookupEnv resolves ${VAR} references.
func Parse(data []byte, lookupEnv LookupEnv) ([]Upstream, []Warning, error) {
	var f struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil, jsonError(data, err)
	}

	var (
		upstreams []Upstream
		warnings  []Warning
		errs      []error
	)
	for _, name := range slices.Sorted(maps.Keys(f.MCPServers)) {
		u, msgs, err := parseUpstream(name, f.MCPServers[name], lookupEnv)
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
	}
	decode := func(name string) error {
		f := known[name]
		if v, ok := fields[name]; ok && json.Unmarshal(v, f.dst) != nil {
			return fmt.Errorf("%q must be %s", name, f.want)
		}
		return nil
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

	if missing := interpolate(&u, lookupEnv); len(missing) > 0 {
		return nil, append(warnings, unsetWarning(missing)), nil
	}
	return &u, warnings, nil
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
// env and headers, only the values) and returns the names of unset variables, sorted.
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
