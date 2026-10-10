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
	c := &Config{Path: abs}
	c.doc, err = ParseDocument(data)
	if err == nil {
		c.Entries, c.Warnings, err = c.doc.Entries(lookupEnv)
	}
	if err != nil {
		return nil, configError(path, err)
	}
	if named {
		for _, e := range c.Entries {
			if e.Upstream != nil {
				e.Upstream.ConfigPath = abs
			}
		}
	}
	return c, readErr
}

// configError is err, a problem with the Config at path, as sprut reports
// it.
func configError(path string, err error) error {
	return fmt.Errorf("config %s: %w", path, err)
}

// Document returns the Config as the user wrote it, to edit. Editing it
// leaves c as it was opened.
func (c *Config) Document() *Document {
	return c.doc.clone()
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
