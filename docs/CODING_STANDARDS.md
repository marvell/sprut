# Coding standards

Judgement calls for review. Anything mechanical lives in `.golangci.yml` instead.

## Wording

- **Glossary terms** (`GLOSSARY.md`): capitalised in comments, docs and test names ("the Upstream", "the Config"); lowercase in error strings and log messages, like any other word there ("upstream skipped", "reading config: ...").

## Process boundary

- **Child processes get an explicit environment.** Set `exec.Cmd.Env` from the env passed into `cli.Run`. A nil `Env` silently inherits sprut's real environment, which goes around seam 1 where no lint can see it.

## Reuse

- **A new helper replaces the existing copies of its logic**, including those outside the diff. Search the package for the same loop or check before accepting a helper, and flag each copy left behind.
