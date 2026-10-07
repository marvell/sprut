## Checks

Before every commit, `make check` must pass: it runs exactly what CI runs.

Only `main` touches `os.Args`, env, std streams and `os.Exit`; tests live only in `internal/cli` and `internal/config`. `make lint` enforces both (`.golangci.yml`).

Test seams come pre-agreed from the spec's Testing Decisions; confirm with the user only a seam the spec doesn't name.

## Review

Judgement-call standards: `docs/CODING_STANDARDS.md`.

## Library gotchas

go-sdk, urfave/cli or os/exec: read `docs/agents/library-gotchas.md` before using any of them.

## Agent skills

- **Issues, specs and tickets** (Linear): `docs/agents/issue-tracker.md`.
- **Triage labels**: the Linear label strings are the canonical role names (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`).
- **Domain concepts and ADRs**: `docs/agents/domain.md`.
