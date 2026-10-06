## Checks

Before every commit, `make check` must pass: it runs exactly what CI runs.

Only `main` touches `os.Args`, env, std streams and `os.Exit`; tests live only in `internal/cli` and `internal/config`. `make lint` enforces both (`.golangci.yml`).

## Library gotchas

go-sdk or urfave/cli: read `docs/agents/library-gotchas.md` before working with either.

## Agent skills

### Issue tracker

Linear: team Personal (key `P`), current project (see the doc). See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
