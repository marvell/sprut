## Checks

Commit only after `make check` exits 0 on the exact tree being committed: it runs exactly what CI runs. Gate the commit on that exit code (`make check && git commit ...`, or a separate run you read first). Any failure blocks the commit until fixed, a flaky test included.

Only `main` touches `os.Args`, env, std streams and `os.Exit`; tests live only in `internal/cli` and `internal/config`. `make lint` enforces both (`.golangci.yml`).

Test seams come pre-agreed from the spec's Testing Decisions; confirm with the user only a seam the spec doesn't name.

## Review

Judgement-call standards: `docs/CODING_STANDARDS.md`.

## Library gotchas

Before writing code against go-sdk (hooks, transports, OAuth), urfave/cli, os/exec, net/http or `flock`, and on any error from them: read `docs/agents/library-gotchas.md`.

## Agent skills

- **Issues, specs and tickets** (Linear): `docs/agents/issue-tracker.md`.
- **Triage labels**: the Linear label strings are the canonical role names (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`).
- **Domain concepts and ADRs**: `docs/agents/domain.md`.
