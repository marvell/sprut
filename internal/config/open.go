package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Config is a Config file as sprut opened it: all of its entries, enabled
// or not, in Config order.
type Config struct {
	// Path is the Config's absolute path.
	Path     string
	Entries  []Entry
	Warnings []Warning

	doc *Document
	// named is whether the Config is at a path the user named, not at the
	// default location.
	named bool
}

// Open reads and parses the Config that flag (from -c/--config) and
// lookupEnv point at, as Path resolves it; lookupEnv also resolves ${VAR}
// references. Unless the Config is at the default location, each resolved
// Upstream carries its absolute path as ConfigPath: the Agent may run sprut
// with -c or $SPRUT_CONFIG that the user's shell lacks, and in another
// directory.
//
// Reading a missing Config fails with an fs.ErrNotExist, and still returns
// the Config as empty, at its path, so that an edit can create it.
func Open(flag string, lookupEnv LookupEnv) (*Config, error) {
	path, named, err := Path(flag, lookupEnv)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, configError(path, err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		readErr = fmt.Errorf("reading config: %w", readErr)
		if !errors.Is(readErr, fs.ErrNotExist) {
			return nil, readErr
		}
		data = []byte("{}")
	}
	doc, err := ParseDocument(data)
	if err != nil {
		return nil, configError(path, err)
	}
	c, err := resolve(abs, named, doc, lookupEnv)
	if err != nil {
		return nil, configError(path, err)
	}
	return c, readErr
}

// Edit applies edit to the Config that flag and lookupEnv point at, as
// Open finds it, and writes the result back with writeFile, returning the
// Config as written. A missing Config is edited as an empty one, and so
// created; a Config that fails to parse is refused, and so is a result
// that does, leaving the file as it was. edit reports whether it changed
// the Config, and must leave it as it was if not: then the file is left
// as it is and Edit returns the Config as opened.
func Edit(flag string, lookupEnv LookupEnv, edit func(*Document) (bool, error)) (*Config, error) {
	c, err := Open(flag, lookupEnv)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	edited, err := edit(c.doc)
	if err != nil {
		return nil, err
	}
	if !edited {
		return c, nil
	}
	if c, err = resolve(c.Path, c.named, c.doc, lookupEnv); err != nil {
		return nil, err
	}
	if err := writeFile(c.Path, c.doc.Bytes()); err != nil {
		return nil, fmt.Errorf("writing config: %w", err)
	}
	return c, nil
}

// resolve is the Config at the absolute path abs that doc holds. Unless
// the Config is at the default location (named is false), each resolved
// Upstream carries abs as its ConfigPath.
func resolve(abs string, named bool, doc *Document, lookupEnv LookupEnv) (*Config, error) {
	entries, warnings, err := doc.Entries(lookupEnv)
	if err != nil {
		return nil, err
	}
	if named {
		for _, e := range entries {
			if e.Upstream != nil {
				e.Upstream.ConfigPath = abs
			}
		}
	}
	return &Config{Path: abs, Entries: entries, Warnings: warnings, doc: doc, named: named}, nil
}

// configError is err, a problem with the Config at path, as sprut reports
// it.
func configError(path string, err error) error {
	return fmt.Errorf("config %s: %w", path, err)
}

// Upstreams returns the Upstreams that the Gateway starts: those neither
// disabled nor skipped, in Config order.
func (c *Config) Upstreams() []Upstream {
	return started(c.Entries)
}

// Lookup returns the entry called name, enabled or not. It fails with
// ErrNotFound if the Config has none.
func (c *Config) Lookup(name string) (Entry, error) {
	i := slices.IndexFunc(c.Entries, func(e Entry) bool { return e.Name == name })
	if i < 0 {
		return Entry{}, fmt.Errorf("upstream %q: %w", name, ErrNotFound)
	}
	return c.Entries[i], nil
}

// Upstream returns the entry called name resolved, enabled or not, so that
// its Credentials can be found. It fails as Lookup does, or with why the
// entry doesn't resolve.
func (c *Config) Upstream(name string) (Upstream, error) {
	e, err := c.Lookup(name)
	switch {
	case err != nil:
		return Upstream{}, err
	case e.Skipped != "":
		return Upstream{}, fmt.Errorf("upstream %q: %s", name, e.Skipped)
	case e.Unresolved != "":
		return Upstream{}, fmt.Errorf("upstream %q: disabled, and %s", name, e.Unresolved)
	}
	return *e.Upstream, nil
}
