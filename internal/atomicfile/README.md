# atomicfile

## Overview

Package `atomicfile` writes files so that a reader sees either the old file whole or the new one whole, never one half-written. It is used by [`credentials`](../credentials/README.md) to replace an Upstream's Credentials file, and by the `config` package to write the Config document. It imports only `os` and `path/filepath`.

## Interface

- `func Write(path string, data []byte, perm os.FileMode) error`: replaces the file at `path` with `data`, with mode `perm`, through a temporary file in the same directory renamed over it.

## Invariants

- The temporary file is created in `filepath.Dir(path)`, never elsewhere, so that the final `os.Rename` stays on one filesystem. The directory must already exist: `Write` does not create it.
- The data is written, synced (`tmp.Sync`) and closed before the rename, and any error before the rename leaves the file at `path` untouched.
- The mode is set with `tmp.Chmod(perm)` before any data is written, so the new file has exactly `perm`.
- Every `Write` makes a new file (a new inode) in place of the old one. Callers rely on this: `credentials` tells that a file was rewritten by comparing files with `os.SameFile`. Do not change `Write` to write in place.
- `Write` takes no lock. Callers that must not interleave writes serialise them themselves, as `credentials` does with its lock file.
- `Write` does not follow a symlink at `path`: the rename replaces the link itself with a regular file and leaves its target untouched. A caller that wants the target written resolves the link first, as the `config` package does (`resolveSymlinks`).

## Side Effects

- Creates a temporary file named `.<base>.*.tmp` beside `path`, and removes it on return if the rename did not happen.
- Calls `fsync` on the temporary file, then renames it over `path`, which replaces any file already there.

## Verification

- Run: `go test ./internal/atomicfile/...` (the package has no test files of its own; it is exercised through `go test ./internal/credentials/... ./internal/config/...`)
