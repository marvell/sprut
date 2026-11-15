# statedir

## Overview

Package `statedir` locates sprut's state directory, where the Credentials and the Audit log live, and makes directories under it private. It is used by [`credentials`](../credentials/README.md) and [`audit`](../audit/README.md). It depends on the `config` package for `config.LookupEnv` and `config.XDGDir`.

## Interface

- `func Dir(lookupEnv config.LookupEnv) (string, bool)`: sprut's state directory, `$XDG_STATE_HOME/sprut` or `~/.local/state/sprut`. It reports `false` if neither `XDG_STATE_HOME` nor `HOME` gives one.
- `func MkdirPrivate(root, dir string) error`: makes `dir`, and every directory from `root` down to it, readable only by the user (`0700`), including those that already exist.

## Invariants

- `Dir` reads the environment only through the `lookupEnv` it is given, never `os.Getenv`: only `main` touches the process environment (see `CLAUDE.md` and ADR-0004).
- `Dir` resolves the base directory with `config.XDGDir(lookupEnv, "XDG_STATE_HOME", ".local/state")` and always appends `sprut`.
- `MkdirPrivate` changes no directory above `root`. It compares `root` and `dir` with `filepath.Rel`, so `root` need not be a cleaned path (a trailing slash or a `..` is fine), and it returns an error, creating and changing nothing, when `dir` is neither `root` nor under it. The comparison is of the paths as written: it does not resolve symlinks.
- `MkdirPrivate` sets `0700` with `os.Chmod` on existing directories too, so the mode does not depend on the umask or on who made the directory first.

## Side Effects

- `Dir` has none: it does not touch the disk.
- `MkdirPrivate` creates directories (`os.MkdirAll`) and changes the mode of `dir` and each of its ancestors up to `root` to `0700`.

## Verification

- Run: `go test ./internal/statedir/...`. `credentials` and `audit` exercise it too: `go test ./internal/credentials/... ./internal/audit/...`
