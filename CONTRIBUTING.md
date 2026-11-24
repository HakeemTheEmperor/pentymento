# Contributing to Pentymento

Thank you for helping. This page covers how to build and test Pentymento, how to write commit messages, and how to sign off your work.

## Licence and sign-off

Pentymento is licensed under the [Apache License 2.0](LICENSE). By contributing, you agree that your contribution is licensed under the same terms. There is no CLA.

Every commit must carry a Developer Certificate of Origin (DCO) sign-off, which states that you have the right to submit the work under the project's licence (see [developercertificate.org](https://developercertificate.org/)). Add it with `-s`:

```sh
git commit -s
```

This appends a line with the name and email from your Git configuration:

```text
Signed-off-by: Your Name <you@example.com>
```

CI rejects commits without a sign-off.

## Commit messages

Commit messages follow the Go project style.

- **Subject:** `<package path>: <lowercase summary>`.
  - The prefix is a real directory in the repository, such as `internal/repo` or `cmd/pentym`.
  - Use `all:` for changes that cut across the codebase and `docs:` for documentation-only changes.
  - The summary starts lowercase and has no trailing full stop.
  - The subject is at most 72 characters.
- **Blank line**, then a **body** that explains what changed and why, wrapped at about 72 characters.
- **References** such as `Fixes #42`, when there are any.
- **Sign-off** line, added by `git commit -s`. It must be in the last paragraph and match the commit author's email.

If you paste a prepared message, save it to a file and commit with `git commit -s -F <file>`, so the sign-off lands at the end rather than above your subject.

CI checks every non-merge commit a pull request adds. Run the same check locally before pushing:

```sh
make commits
```

By default it checks `origin/main..HEAD`; set `RANGE=<base>..<head>` to check another range.

Example:

```text
internal/repo: commit snapshot only after a clean finish

A snapshot was written as soon as the source stream ended, even when
the child process later exited with an error. Write the index files
and the snapshot object only after the stream and the child process
have both finished cleanly, so a partial stream never becomes a
snapshot.

Fixes #42

Signed-off-by: Your Name <you@example.com>
```

A documentation change:

```text
docs: explain the anchor snapshot in the retention guide

Signed-off-by: Your Name <you@example.com>
```

## Source files

Every Go source file, and every other file that supports comments, starts with the SPDX licence header and copyright line:

```go
// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi
```

Pentymento is pure Go: no cgo.

## Building and testing

You need Go (the version in `go.mod`) and `make`.

```sh
go build ./...
make test
make lint
```

| Target | Runs |
| --- | --- |
| `make test` | Unit tests, with the race detector where cgo is available |
| `make test-integration` | Unit tests plus database integration tests (needs Docker) |
| `make bench` | Microbenchmarks |
| `make lint` | `go vet`, `golangci-lint`, `staticcheck` and the licence-header check |
| `make commits` | Commit-message and DCO check over `RANGE` |
| `make licenses` | Dependency licence check (`go-licenses`) |
| `make vulncheck` | Known vulnerabilities (`govulncheck`) |
| `make gosec` | Security static analysis (`gosec`) |

Tool versions are pinned in the `Makefile`, and CI runs the same targets.

Integration tests carry the `integration` build tag, so `make test` never needs Docker. They start Postgres and MongoDB containers through `testcontainers-go`. Choose the versions with environment variables; by default every supported version runs:

```sh
PENTYM_TEST_POSTGRES=17 PENTYM_TEST_MONGODB=8.0 make test-integration
```

### Continuous integration

| Pipeline | Runs | When |
| --- | --- | --- |
| `pr` | Lint, unit tests with `-race` on Linux, macOS and Windows, commits and DCO, licences, `govulncheck`, `gosec`, integration on Postgres 17 and MongoDB 8.0, benchmarks | Every pull request to `main` |
| `nightly` | Integration on every supported Postgres and MongoDB version | Daily at 02:00 UTC, or by hand |
| `release` | Release gates | Pushing a `v*` tag |

### Windows

- Run the commands above from Git Bash.
- Database integration tests run through Docker Desktop with WSL2. Running `make test-integration` inside WSL is usually smoother.
- The race detector needs a C compiler. Without one, `make test` runs without `-race`; run `make test` inside WSL to include it.
