# Black-box tests in any package

Until now, tests could live only in `internal/cli` and `internal/config`, and depguard stopped them from importing the other internal packages. That kept every test behind the program's entry point, but a deep module with an interface of its own, such as `credentials.Source`, could only be tested through `cli.Run`. Its lock, rename and rotation protocol was therefore checked with real processes and sleeps rather than deterministically. We now allow tests in any package, as Go projects usually do, but only black-box ones: every test file is in an external `_test` package and reaches its package through the exported API alone, which `testpackage` in `.golangci.yml` enforces. The one exception is the test of `main`, which is in package `main` only to run `main` itself as a process of its own, under testscript. Test files may import any package of this module, including shared fakes. The process boundary does not change: only `main` touches `os.Args`, env, the standard streams and `os.Exit`.

## Considered Options

- **Tests only in `internal/cli` and `internal/config`** (the v0.1 spec's Testing Decisions, which this supersedes): it keeps tests off internals, but it also keeps them off the interfaces of deep modules.
- **White-box tests allowed**: easier to reach odd states, but they couple tests to implementation details, so refactors break them. That is the reason the old rule existed.
- **`//go:build integration` tags for slow tests**: gopls and `go vet` don't see code under the tag, and every sprut test is hermetic and fast, so no split by tag is needed.
