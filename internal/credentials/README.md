# credentials

## Overview

Package `credentials` stores the Credentials of each OAuth Upstream in a file of its own, written only under a lock on a sibling file ([ADR-0003](../../docs/adr/0003-credentials-in-locked-files.md)). It has two layers:

- `Store` (`credentials.go`): the directory of Credentials files, with `Load`, `Save` and `Delete`, and the cross-process lock.
- `Source` (`source.go`): the Credentials of one Upstream as one process uses them. It reads them again whenever their file changes and renews them only once at a time, both in this process and, through the Store's lock, across processes.

The `gateway` package is its only user: it supplies the `Refresh` that talks to the Authorization Server, and saves what a Login leaves behind. The package itself makes no network call. It uses [`atomicfile`](../atomicfile/README.md) to replace a Credentials file and [`statedir`](../statedir/README.md) to locate and protect the directory, and depends on the `config` package for `config.Upstream` and `config.LookupEnv`.

## Interface

Credentials and their Binding:

- `type Credentials struct`: what a Login leaves behind for one Upstream. It embeds `oauth2.Token` and adds `Scopes` (granted), `TokenURL`, `AuthStyle`, `ClientID` and `ClientSecret` (the client that Dynamic Client Registration issued, if any), `Resource`, `Issuer` and `Config` (a `Binding`).
- `func (c *Credentials) Dead() bool`: whether `c` can't be used any more without a new Login: the access token has expired, and there is no refresh token.
- `func (c *Credentials) Expired() bool`: whether the access token has expired. A zero `Expiry` never expires.
- `type Binding struct`: what an Upstream's Config said when its Credentials were issued, and must still say for them to be used. Fields `URL`, `ClientID` (`oauth.clientId`), `Scopes` (`oauth.scopes`).
- `func BindingOf(u config.Upstream) Binding`: the Binding of `u` as its Config is now.

Store:

- `type Store struct`: the directory holding the Credentials of every Upstream.
- `func NewStore(lookupEnv config.LookupEnv) (*Store, error)`: the Store under sprut's state directory, `$XDG_STATE_HOME/sprut/credentials` or `~/.local/state/sprut/credentials`.
- `var ErrNoHome`: why `NewStore` can't return a Store.
- `func (s *Store) Load(u config.Upstream) (*Credentials, error)`: the Credentials of `u`, or `nil` if it has none, or none issued for `u` as its Config is now.
- `func (s *Store) Save(ctx context.Context, upstream string, c *Credentials) error`: replaces the Credentials of `upstream` with `c`, under its lock.
- `func (s *Store) Delete(ctx context.Context, upstream string) (bool, error)`: deletes the Credentials of `upstream` under its lock, and reports whether there were any.
- `func (s *Store) Wait()`: waits until no renewal that holds the lock is in progress, so that rotated Credentials are saved before the process exits.
- `func (s *Store) Source(u config.Upstream, refresh Refresh, log *slog.Logger) *Source`: the Source of `u`'s Credentials, which renews them with `refresh` and logs to `log`.

Source:

- `type Source struct`: the Credentials of one Upstream as one process uses them.
- `func (s *Source) Token(ctx context.Context) (string, error)`: the access token to send, renewed first if it expires within 5 minutes (`renewBefore`), or `""` without Credentials. A renewal that fails leaves the access token to send until it expires, unless the Credentials need a Login.
- `func (s *Source) Rejected(ctx context.Context, token, metadata string) (string, error)`: renews the Credentials whose access token the Upstream answered 401 to, at most once for each token, and returns the new access token. `metadata` is the URL of the Protected Resource Metadata that the 401 named, if any.
- `func (s *Source) Unauthorized() error`: why the Upstream needs a Login when it answers 401 past `Token` and `Rejected`.
- `type Refresh func(ctx context.Context, c *Credentials, metadata string) (*Credentials, error)`: gets a new access token for `c` from their Authorization Server and returns the Credentials it makes. It returns `ErrRefreshRejected` when the server rejects the refresh token, and a `*LoginNeededError` when `c` can't be used any more; any other error leaves them as they are.

Errors:

- `type LoginNeededError struct { Reason string }`: the error of an Upstream that has no Credentials it can use. `Reason` is why the Credentials it had can't be used, `""` if it had none.
- `const ReasonExpired`: the `Reason` of Credentials that are dead (`Credentials.Dead`).
- `var ErrRefreshRejected`: what a `Refresh` returns when the Authorization Server rejects the refresh token (`invalid_grant`).

## Invariants

Files and the lock:

- An Upstream named `<name>` has two files in the Store's directory: `<name>.json`, its Credentials, and `<name>.lock`. The name is used as it is (`Store.path`).
- Every write and delete of `<name>.json` happens under an exclusive `flock` on `<name>.lock` (`Store.lock`), held by one process at a time. The lock is on a file of its own, never renamed or deleted, because a lock on the Credentials file would stay on the inode that a write replaces. `Delete` leaves the lock file in place.
- Waiting for the lock is bounded by `lockTimeout` (30 seconds) and by `ctx`. `flock` can't be interrupted, so it is retried without blocking (`LOCK_NB`), 1 to 50 milliseconds apart.
- `Load` takes no lock. It is safe because `lock.save` writes through `atomicfile.Write`: a reader sees either the old file whole or the new one.
- Every write makes a new file. `fileVersion` depends on it: a Source keeps the file it last read open and compares it with `os.SameFile`, so a file written in place would not be noticed. The file is kept open so that the system can't give its inode to a later write.
- `Delete` of Credentials that don't exist takes no lock and so creates no lock file or directory.

Binding:

- `Load` returns `nil, nil` both when there is no file and when the stored `Config` Binding differs from `BindingOf(u)`. Credentials issued for another URL, client id or set of scopes are never used.
- Bindings are compared without regard to the order of the scopes, but `nil` scopes and empty scopes differ (`Binding.equal`).
- `oauth.clientSecret` is left out of the Binding on purpose, so that rotating it keeps the Credentials.
- `Save` does not check the Binding: the caller sets `Credentials.Config`.

Renewal:

- In one process, every request that needs a renewal at the same time waits for the same one (`Source.renewing`). Across processes, `Store.renew` runs the `Refresh` under the Upstream's lock and reads the Credentials again first: if their access token is no longer the stale one, another process has renewed them, and they are returned without a refresh.
- A renewal goes on even if every request gives up waiting (`context.WithoutCancel`): the Authorization Server may already have rotated the refresh token, and the new one must be kept. Each request waits only as long as its own `ctx` allows.
- The process must call `Store.Wait` before it exits, so that a renewal in progress saves what it got. `Wait` counts only renewals that already hold the lock; one that takes it after `Wait` has returned is not waited for.
- When `Refresh` returns `ErrRefreshRejected`, the Credentials are read again: if another process has rotated the refresh token meanwhile, those are used; otherwise the Source is dead with a `*LoginNeededError`.
- When renewed Credentials can't be saved, they are still used by this process, and the failure is logged as a warning.
- A `*LoginNeededError` from a renewal is kept (`Source.dead`) and returned by every later `Token` and `Rejected` until the Credentials file changes, which the next `Token` notices. Any other error leaves the Credentials as they are, and the next request tries again.
- `Token` renews only Credentials with a non-zero `Expiry` that is less than `renewBefore` away. Credentials without a refresh token are sent until they expire, then are dead (`ReasonExpired`).
- A renewal that `Token` starts inside that window and that fails for a reason other than a needed Login does not fail the request: `Token` returns the access token it has, which is good until it expires, and logs the failure as a warning. The next request tries again. Once the access token has expired, `Token` returns the renewal's error, `renewing credentials: ...`. `Rejected` always returns it: the Upstream has refused that token (`Source.Token`, `Source.renewShared`).
- A `Refresh` must bound its own time. It gets a context without cancellation or deadline (`context.WithoutCancel`) and runs while `Store.renew` holds the Upstream's lock, so one that hangs keeps every other process waiting for `lockTimeout`, and keeps `Store.Wait`, which has no timeout, from returning.
- `Rejected` renews at most once for each access token: when the Upstream also answers 401 to the token that a renewal on a 401 got (`Source.rejected`), the result is a `*LoginNeededError`.
- A Source reads the Credentials again whenever their file is no longer the one it read (`Source.reload`, on every `Token`). That is how a running `sprut serve` picks up a Login, a logout or another process's renewal without a restart.
- `Source.creds` is replaced, never changed: a `*Credentials` read under `mu` may be used after the lock is released.
- `Source` and `Store` are safe for concurrent use.
- Nil receivers: `Token` on a nil `*Source` returns `""` and no error, and `Wait` on a nil `*Store` returns at once. `Rejected` and `Unauthorized` need a non-nil Source.

## Side Effects

- `NewStore` has none: it only resolves the state directory through `lookupEnv`.
- `Load`, `Token` and `Rejected` read `<name>.json`; `Token` also calls `os.Stat` on it each time, to see whether it changed.
- `Save`, `Delete` of existing Credentials, and every renewal create the state directory and its `credentials` directory if needed and set both to mode `0700` (`statedir.MkdirPrivate`), create `<name>.lock` with mode `0600`, and hold an exclusive `flock` on it.
- `Save` and a renewal replace `<name>.json`, mode `0600`, through a temporary file renamed over it. The file holds the access token, the refresh token and any client secret.
- `Delete` removes `<name>.json`.
- A Source keeps one file descriptor open, on the Credentials file it last read.
- A renewal runs in a goroutine of its own, calls the `Refresh` it was given (which is where any network call happens), and may block for up to `lockTimeout` waiting for another process.
- A renewal whose Credentials can't be saved writes one WARN record, `renewed credentials not saved`, with the Upstream's name and the error, to the `slog.Logger`.
- Each `Token` that sends an access token it could not renew writes one WARN record, `credentials not renewed; sending the access token until it expires`, with the Upstream's name and the error.
- `flock` comes from `syscall.Flock`, so the package builds only where that exists (the release targets are Linux and macOS).

## Verification

- Run: `go test ./internal/credentials/...`
