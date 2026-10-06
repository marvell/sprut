## Checks

Before every commit, `make check` must pass: it runs exactly what CI runs.

`make lint` (part of `make check`) runs golangci-lint, which enforces the spec's seam rules; see `.golangci.yml`.

## Agent skills

### Issue tracker

Linear: team Personal (key `P`), current project (see the doc), via the Linear MCP tools. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `GLOSSARY.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.
