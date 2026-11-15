# chars

## Overview

Package `chars` counts and cuts text in characters (runes), not bytes, so that a cut never splits a multi-byte character. It is the one helper behind every "keep the first N characters" limit: the output limit of [`sandbox`](../sandbox/README.md) and the error limit of [`audit`](../audit/README.md). It imports only `unicode/utf8`.

## Interface

- `func Cut(s string, n int) (kept string, dropped int)`: `kept` is `s` cut to its first `n` characters, and `dropped` is how many characters were left out.

## Invariants

- `Cut` counts characters, never bytes: `kept` always ends on a character boundary of `s`, and `dropped` is `utf8.RuneCountInString` of the rest.
- `kept` is a prefix of `s` (a slice of it, not a copy), and `dropped` is 0 exactly when `kept == s`.
- `n == 0` keeps nothing and drops every character of `s`.
- `n` is expected to be 0 or more. A negative `n` never reaches the cut, so `Cut` returns `s` whole with `dropped == 0`.

## Side Effects

- None: `Cut` is a pure function that does not allocate, write or lock.

## Verification

- Run: `go test ./internal/chars/...` (the package has no test files of its own; it is exercised through `go test ./internal/sandbox/... ./internal/audit/...`)
