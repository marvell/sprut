# Credentials in per-Upstream files, shared through a file lock

Several `sprut serve` processes use the same Upstream at once (ADR-0001), and OAuth servers such as Atlassian's rotate the refresh token on every refresh, so two processes renewing one Upstream's Credentials concurrently would invalidate each other and force a Login. Credentials therefore live in one 0600 JSON file per Upstream under `$XDG_STATE_HOME/sprut/credentials/`, and a process renews them only while holding an exclusive `flock` on a sibling `.lock` file: it re-reads the Credentials under the lock, uses a token another process already renewed, and otherwise renews and writes atomically (temp file and rename). The lock is on a separate file because `flock` locks an inode, and the rename replaces the Credentials file's inode, so a lock on it would let two processes hold "the" lock at once; the `.lock` file is therefore never renamed or deleted, not even by a logout. We accept file-based secrets in exchange for keeping one static binary and no daemon.

## Considered Options

- **OS keychain** (macOS Keychain, libsecret): better at-rest protection, but libsecret needs D-Bus on Linux, breaks the single static binary, and still needs its own cross-process locking.
- **Shared daemon that owns the Credentials**: a single refresher with no locking, but it is the daemon ADR-0001 rejected.
- **No coordination**: concurrent refreshes with rotating refresh tokens end in `invalid_grant` and a forced Login, which is exactly what this feature must prevent.
