package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marvell/sprut/internal/atomicfile"
)

// Document is a Config as the user wrote it, to edit and write back. It
// keeps every entry's JSON, the order of entries and the keys sprut doesn't
// know; writing it back changes only their layout.
type Document struct {
	members []member
	// serversAt is the index in members of the mcpServers that sprut
	// reads, or -1 when there is none.
	serversAt int
	servers   servers
}

// member is one key of a JSON object, with its value.
type member struct {
	key   string
	value json.RawMessage
}

// Definition is an Upstream as the user writes it into the Config, with
// ${VAR} references left in. Its JSON is the entry as written.
type Definition struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	OAuth   *OAuth            `json:"oauth,omitempty"`
	// Disabled is written only when true: "disabled": false is the same as
	// none.
	Disabled bool `json:"disabled,omitempty"`
}

// ErrExists is the error of adding an Upstream whose name is taken.
var ErrExists = errors.New("already in the config")

// ErrNotFound is the error of naming an Upstream the Config doesn't have.
var ErrNotFound = errors.New("not in the config")

// ParseDocument parses Config bytes into a Document. It fails where
// ParseEntries would on the Config's shape, but checks no entry.
func ParseDocument(data []byte) (*Document, error) {
	if !json.Valid(data) {
		var v any
		return nil, jsonError(data, json.Unmarshal(data, &v))
	}
	members, err := decodeObject(data)
	if err != nil {
		return nil, errors.New("the config must be a JSON object")
	}
	d := &Document{members: members, serversAt: -1}
	for i, m := range members {
		// As encoding/json matches a struct field: in any case, the last
		// match winning.
		if strings.EqualFold(m.key, "mcpServers") {
			d.serversAt = i
		}
	}
	if d.serversAt >= 0 {
		if d.servers, err = parseServers(members[d.serversAt].value); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// Add adds the Upstream name, defined by def, after the others. If the
// name is taken, Add fails with ErrExists, or replaces that entry in its
// place when replace is set.
func (d *Document) Add(name string, def Definition, replace bool) error {
	_, exists := d.servers.entries[name]
	if exists && !replace {
		return fmt.Errorf("upstream %q: %w", name, ErrExists)
	}
	entry, err := marshal(def)
	if err != nil {
		return err
	}
	if d.servers.entries == nil {
		d.servers.entries = map[string]json.RawMessage{}
	}
	if !exists {
		d.servers.names = append(d.servers.names, name)
	}
	d.servers.entries[name] = entry
	d.setServers()
	return nil
}

// Remove removes the Upstream name, keeping the others in their order.
// It fails if there is no Upstream of that name.
func (d *Document) Remove(name string) error {
	if _, ok := d.servers.entries[name]; !ok {
		return fmt.Errorf("upstream %q: %w", name, ErrNotFound)
	}
	delete(d.servers.entries, name)
	d.servers.names = slices.DeleteFunc(d.servers.names, func(n string) bool { return n == name })
	d.setServers()
	return nil
}

// setServers writes d.servers back into mcpServers, adding it after the
// other keys if there is none.
func (d *Document) setServers() {
	entries := make([]member, len(d.servers.names))
	for i, n := range d.servers.names {
		entries[i] = member{n, d.servers.entries[n]}
	}
	if d.serversAt < 0 {
		d.members = append(d.members, member{key: "mcpServers"})
		d.serversAt = len(d.members) - 1
	}
	d.members[d.serversAt].value = object(entries)
}

// Bytes returns the Config as JSON indented by two spaces, ending in a
// newline.
func (d *Document) Bytes() []byte {
	var out bytes.Buffer
	// Every value is valid JSON, so Indent can't fail.
	_ = json.Indent(&out, object(d.members), "", "  ")
	out.WriteByte('\n')
	return out.Bytes()
}

// object is the JSON object of members, in order.
func object(members []member) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := marshal(m.key) // a string always marshals
		b.Write(key)
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// marshal is json.Marshal, but leaves <, > and & as they are, the way the
// user types them.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// WriteFile replaces the Config at path with data atomically, so that the
// Config is never seen half-written. A Config that is a symlink is written
// at its target. An existing Config keeps its mode; a new one is created
// 0600, in directories created 0700, since it may hold secrets.
func WriteFile(path string, data []byte) error {
	path, err := resolveSymlinks(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	switch fi, err := os.Stat(path); {
	case err == nil:
		mode = fi.Mode().Perm()
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
	default:
		return err
	}
	return atomicfile.Write(path, data, mode)
}

// resolveSymlinks follows path while it is a symlink, to the file it
// names, which may not exist yet: unlike filepath.EvalSymlinks, it can
// resolve a symlink to a Config still to be created.
func resolveSymlinks(path string) (string, error) {
	for range 40 { // as many as Linux follows
		fi, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) || err == nil && fi.Mode()&fs.ModeSymlink == 0 {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}
