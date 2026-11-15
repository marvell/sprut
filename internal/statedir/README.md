# statedir

## Overview

Package `statedir` locates sprut's state directory, where the Credentials and the Audit log live, and makes directories under it private. It is used by [`credentials`](../credentials/README.md) and [`audit`](../audit/README.md). It depends on the `config` package for `config.LookupEnv` and `config.XDGDir`.

## Interface

- `func Dir(lookupEnv config.LookupEnv) (string, bool)`: sprut's state directory, `$XDG_STATE_HOME/sprut` or `~/.local/state/sprut`. It reports `false` if neither `XDG_STATE_HOME` nor `HOME` gives one.
- `func MkdirPrivate(root, dir string) error`: makes `dir`, and every directory from `root` down to it, readable only by the user (`0700`), including those that already exist.

## Invariants

- `Dir` reads the environment only through the `lookupEnv` it is given, never `os.Getenv`: only `main` touches the process environment (see `CLAUDE.md` and ADR-0004).
- `Dir` resolves the base directory with `config.XDGDir(lookupEnv, "XDG_STATE_HOME", ".local/state")` and always appends `sprut`.
- `MkdirPrivate` requires `dir` to be `root` or a directory under it. It walks up from `dir` with `filepath.Dir` and stops at `root`; given a `dir` outside `root`, it would `chmod` every ancestor up to the filesystem root.
- `MkdirPrivate` sets `0700` with `os.Chmod` on existing directories too, so the mode does not depend on the umask or on who made the directory first.

## Side Effects

- `Dir` has none: it does not touch the disk.
- `MkdirPrivate` creates directories (`os.MkdirAll`) and changes the mode of `dir` and each of its ancestors up to `root` to `0700`.

## Verification

- Run: `go test ./internal/statedir/...` (the package has no test files of its own; it is exercised through `go test ./internal/credentials/... ./internal/audit/...`)
