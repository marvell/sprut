# config

## Overview

Package `config` turns a Config file (JSON with an `mcpServers` object) into the
Upstream definitions it declares, and edits that file in place.

It has three layers:

- `config.go`: locating the Config (`Path`, `XDGDir`), and parsing its bytes into
  entries and resolved Upstreams (`Parse`, `ParseEntries`, `Document.Entries`),
  including transport inference, field checks and `${VAR}` interpolation.
- `document.go`: `Document`, the Config as the user wrote it, which can be
  edited (`Add`, `Remove`, `SetDisabled`) and serialized back (`Bytes`) without
  losing unknown keys or entry order. It also holds the unexported `writeFile`
  and `resolveSymlinks`, which write the file for `Edit`.
- `open.go`: `Open` and `Edit`, the only exported functions that touch the
  filesystem. They tie the two layers together into a `Config`.

The package never reads the process environment itself. Every environment
lookup goes through a `LookupEnv` the caller passes in. In the current tree its
functions are called by `internal/cli` (`Open`, `Edit`, `LookupIn`),
`internal/gateway` (`LookupIn`) and `internal/statedir` (`XDGDir`). Its types
are used more widely: `internal/gateway` takes `Upstream` and `Entry`,
`internal/credentials` takes `Upstream` and `LookupEnv`, and `internal/audit`
takes `LookupEnv`.

## Interface

### Locating the Config

- `type LookupEnv func(string) (string, bool)`: an environment lookup shaped
  like `os.LookupEnv`.
- `func LookupIn(env []string) LookupEnv`: a lookup over a slice in
  `os.Environ` form. The last entry for a key wins; entries without `=` are
  dropped.
- `func Path(flag string, lookupEnv LookupEnv) (path string, named bool, err error)`:
  resolves which Config file to read, in this order: `flag` (from
  `-c`/`--config`), `$SPRUT_CONFIG`, `$XDG_CONFIG_HOME/sprut/config.json`,
  `~/.config/sprut/config.json`. `named` is true only for the first two. It
  fails when neither a usable `XDG_CONFIG_HOME` nor `HOME` is set.
- `func XDGDir(lookupEnv LookupEnv, variable, fallback string) (string, bool)`:
  the base directory an XDG variable names, or `$HOME/<fallback>`. Reports
  false if neither is set.

### Parsing

- `func Parse(data []byte, lookupEnv LookupEnv) ([]Upstream, []Warning, error)`:
  the Upstreams the Gateway starts, in Config order, plus warnings. Leaves
  `Upstream.ConfigPath` empty.
- `func ParseEntries(data []byte, lookupEnv LookupEnv) ([]Entry, []Warning, error)`:
  every entry of the Config, started or not, in Config order.
- `type Transport string` with constants `Stdio` (`"stdio"`) and `HTTP`
  (`"http"`).
- `type Upstream struct`: one resolved definition. Fields `Name`, `Transport`;
  for stdio `Command`, `Args`, `Env`; for HTTP `URL`, `Headers`, `OAuth`
  (nil when the entry has no `oauth`); and `ConfigPath`.
- `type OAuth struct`: `ClientID`, `ClientSecret`, `Scopes`. Its JSON tags
  (`clientId`, `clientSecret`, `scopes`) are the Config's `oauth` object.
- `type Warning struct`: `Upstream` (the entry name) and `Message`.
- `type Entry struct`: one entry of the Config. `Name`, `Transport` (the
  entry's `type`, or the inferred transport when it has none), `Command`,
  `Args`, `URL` (with `${VAR}` references left in), `Disabled`, `Skipped`,
  `Unresolved`, and `Upstream *Upstream`.
- `func (e Entry) Starts() bool`: true when the entry is not disabled and has a
  resolved `Upstream`.

### Editing

- `type Document struct`: an opaque, editable Config. No exported fields.
- `func ParseDocument(data []byte) (*Document, error)`: checks the Config's
  shape but checks no entry. The Config must be valid JSON and a top-level
  object or `null`; `mcpServers`, if present, must be an object or `null`
  (`decodeObject`). A Config without `mcpServers`, or with a `null` one, has no
  entries.
- `func (d *Document) Entries(lookupEnv LookupEnv) ([]Entry, []Warning, error)`:
  `ParseEntries` over the Document as edited so far.
- `func (d *Document) Add(name string, def Definition, replace bool) error`:
  appends an entry, or replaces an existing one in its place when `replace` is
  set. Fails with `ErrExists` otherwise.
- `func (d *Document) Remove(name string) error`: fails with `ErrNotFound`.
- `func (d *Document) SetDisabled(name string, disabled bool) (bool, error)`:
  reports whether it changed the entry. Fails with `ErrNotFound`, or when the
  entry is not a JSON object or its `disabled` is not a boolean.
- `func (d *Document) Bytes() []byte`: the Config as JSON indented by two
  spaces, ending in a newline.
- `type Definition struct`: an Upstream as written into the Config. `Command`,
  `Args`, `Env`, `URL`, `Headers`, `OAuth`, `Disabled`; every field is
  `omitempty`.
- `func (def Definition) Transport() Transport`: the transport the parser
  would infer, or `""` when `Command` and `URL` are both set or both empty.
- `var ErrExists`, `var ErrNotFound`: sentinel errors, wrapped as
  `upstream "<name>": ...`. Match them with `errors.Is`.

### Opening and writing the file

- `type Config struct`: exported fields `Path` (absolute), `Entries`,
  `Warnings`.
- `func Open(flag string, lookupEnv LookupEnv) (*Config, error)`: reads and
  parses the Config that `Path` resolves.
- `func Edit(flag string, lookupEnv LookupEnv, edit func(*Document) (bool, error)) (*Config, error)`:
  opens the Config, applies `edit`, re-parses the result, and writes it back.
- `func (c *Config) Upstreams() []Upstream`: the entries that start, in Config
  order.
- `func (c *Config) Lookup(name string) (Entry, error)`: the entry of that
  name, enabled or not, or `ErrNotFound`.
- `func (c *Config) Upstream(name string) (Upstream, error)`: the entry
  resolved, enabled or not. Fails as `Lookup` does, or with the entry's
  `Skipped` or `Unresolved` reason.

## Invariants

### Names and order

- An Upstream name must match `^[a-z0-9-]+$` (`validName`). This keeps the
  `__` separator of Namespaced tool names out of Upstream names.
- Entries are always reported in Config order. A name given twice keeps its
  last entry and its first position (`parseServers`).
- The `mcpServers` key is matched case-insensitively, and the last match wins
  (`ParseDocument`), mirroring how `encoding/json` matches a struct field.

### Transport and fields

- An entry's `type` is `"stdio"`, `"http"`, `"sse"`, absent or `""`. Any other
  value is an error. `"sse"` is not an error: the entry is skipped with a
  warning.
- Without `type`, or with `"type": ""`, the transport is inferred from whichever of `command` and
  `url` is set (`inferTransport`). Setting both, or neither, is an error.
- With `type` set, the entry must set the target its transport needs and not
  the other one (`checkTarget`).
- `args` and `env` apply to stdio only; `headers` and `oauth` apply to HTTP
  only. A field on the wrong transport, or a field the parser does not know, is
  ignored with a warning, never an error.
- A `null` in a field the parser decodes (the value itself, an array element,
  or an object value) is rejected (`decodeField`, `hasNull`), because
  `encoding/json` would silently decode it as a zero value.
- `oauth.clientSecret` without `oauth.clientId` is dropped with a warning
  (`parseOAuth`).

### Interpolation

- `${VAR}` references are expanded in `args`, `env` values, `url`, `headers`
  values, and all of `oauth` (`interpolate`). `command` and map keys are never
  interpolated.
- A reference to an unset variable skips the entry with a warning; it is not
  an error. A variable set to the empty string counts as set.
- An HTTP entry whose `url` is empty after interpolation is skipped.
- `Entry.Command`, `Entry.Args` and `Entry.URL` keep the text as written.
  Only `Entry.Upstream` holds interpolated values.
- `Entry.Args` is filled only for a stdio entry that got as far as its
  transport's fields. It is nil for an HTTP entry, for a skipped `"sse"` entry,
  and for a disabled entry whose `type` or target is wrong, even if the entry
  writes `args`.
- `Entry.Transport` is not always `Stdio` or `HTTP`: it is `"sse"` for a skipped
  SSE entry, and for a disabled entry it may be an unknown `type` as written, or
  `""` when the transport can't be inferred.

### Entry states

- Exactly one of these holds for an entry returned by `ParseEntries`:
  it starts (`Starts()` is true); it is enabled and skipped (`Skipped` is set,
  `Upstream` is nil); or it is disabled (`Skipped` is always `""`).
- A disabled entry is resolved as if it were enabled, so its Credentials can
  be found, but its problems never fail the parse and never produce warnings.
  They land in `Unresolved` instead, with `Upstream` nil (`parseEntry`).
- That leniency starts only once `disabled` has been decoded. An invalid name,
  an entry that is not a JSON object, or a wrongly typed `type`, `disabled`,
  `command` or `url` fails the parse even for a disabled entry
  (`resolveEntry`).
- All entry errors of one Config are reported together, each prefixed with
  `upstream "<name>":` (`Document.Entries`, via `errors.Join`).

### Documents and files

- Writing a `Document` back keeps unknown top-level keys, the order of keys
  and entries, and each untouched entry's JSON. Besides layout, it changes
  this: keys are decoded and encoded again (`decodeObject`, `object`), so an
  escape in a key is normalised; a top-level `null` is written as `{}`; and
  after an edit, a `null` `mcpServers` becomes an object, and a name that was
  given twice is written once, with its last entry (`setServers`).
- `Document.Add` does not validate the name or the `Definition`. Validation
  happens when the Document is parsed again, which `Edit` does before writing.
- `SetDisabled(name, false)` removes the `disabled` key instead of writing
  `false`; `Definition.Disabled` is likewise written only when true.
- JSON written by `Add` leaves `<`, `>` and `&` unescaped (`marshal`).
- `Open` on a missing file returns both a non-nil, empty `Config` at the
  resolved path and an error that satisfies `errors.Is(err, fs.ErrNotExist)`.
  Every other failure returns a nil `Config`.
- `Upstream.ConfigPath` is set to the Config's absolute path only when the
  path was named by flag or `$SPRUT_CONFIG`. At the default location it stays
  `""` (`resolve`).
- The `edit` callback passed to `Edit` must return false only if it left the
  Document untouched. `Edit` then writes nothing and returns the Config as
  opened.
- `Edit` refuses a Config that fails to parse, and refuses a result that fails
  to parse, leaving the file as it was in both cases.
- `XDGDir` ignores a relative path in the XDG variable, as the XDG spec
  requires.

## Side Effects

- `Parse`, `ParseEntries`, `ParseDocument`, `Path`, `XDGDir`, `LookupIn` and
  every `Document` and `Config` method do no I/O.
- `Open` reads the Config file with `os.ReadFile`, and resolves its absolute
  path with `filepath.Abs`, which depends on the process working directory for
  a relative path.
- `Edit` does what `Open` does, then, if the callback reported a change, writes
  the file through the unexported `writeFile`:
  - follows a chain of up to 39 symlinks to the file they name, even when the
    final target does not exist yet (`resolveSymlinks` looks at 40 paths, the
    last of which must not be a symlink), so a symlinked Config is written at
    its target;
  - keeps the permission bits of an existing file; creates a new file with
    mode `0600`, and missing parent directories with mode `0700`;
  - replaces the file atomically through `atomicfile.Write` (package
    `internal/atomicfile`): a temporary file in the same directory, synced,
    then renamed over the Config.
- `Edit` is a read-modify-write with no lock taken in this package. Two
  concurrent edits each write a whole file, and the last rename wins.
- No network access, no goroutines, and no package-level mutable state.

## Verification

- Run: `go test ./internal/config/...`
- Fuzzing, outside the regular check: `mise run fuzz` (runs `FuzzParse`).
