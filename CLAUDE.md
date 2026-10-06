## Checks

Before every commit, `make check` must pass: it runs exactly what CI runs.

Only `main` touches `os.Args`, env, std streams and `os.Exit`; tests live only in `internal/cli` and `internal/config`. `make lint` enforces both (`.golangci.yml`).

## Implementing a ticket

1. Test seams come pre-agreed from the spec's Testing Decisions; confirm with the user only a seam the spec doesn't name.
2. Commit before `/code-review` and review from the pre-work SHA: it diffs commits, so uncommitted and untracked files escape it. Review fixes go in a follow-up commit.
3. Post the decisions the ticket left open as a comment on the issue, so the reviewer and later tickets see them.
4. Finish by moving the issue to Done with a closing comment (`docs/agents/issue-tracker.md`).

## Review

Judgement-call standards: `docs/CODING_STANDARDS.md`.

## Library gotchas

go-sdk or urfave/cli: read `docs/agents/library-gotchas.md` before working with either.

## Agent skills

- **Issues, specs and tickets** (Linear): `docs/agents/issue-tracker.md`.
- **Triage labels**: the Linear label strings are the canonical role names (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`).
- **Domain concepts and ADRs**: `docs/agents/domain.md`.
