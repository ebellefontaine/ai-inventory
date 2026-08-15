# Contributing to AI Inventory

Thanks for considering it. This document covers sign-off, tests, and the few
conventions we hold to.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Sign your commits (DCO)

Every commit must carry a `Signed-off-by` line:

```bash
git commit -s -m "feat: add heartbeat endpoint"
```

That line asserts the [Developer Certificate of Origin](https://developercertificate.org/):
you wrote the patch, or you have the right to submit it under the project's
license. CI rejects unsigned commits. If you forget, `git rebase --signoff main`
fixes a branch and `git commit -s --amend` fixes the last commit.

**Why DCO and not a CLA.** A CLA asks contributors to assign rights to a company
so that company can later relicense. It creates real friction — legal review,
a signing ceremony, an account linked to an employer — and we would only need it
if we intended to relicense. We do not. The DCO gets the same assurance about
provenance with none of the ceremony.

## Licensing of contributions

The repository is dual-licensed by directory, and your contribution is licensed
under whichever applies to the files you touch:

| Path | License |
|---|---|
| `sdk/` — Python and TypeScript SDKs, MCP client | Apache-2.0 |
| Everything else — server, UI, connectors | AGPL-3.0 |

This split is deliberate; see [sdk/README.md](sdk/README.md). Please do not move
code across that boundary in either direction without raising an issue first —
copying AGPL server code into an SDK would relicense it by accident.

## Getting set up

You need Go (the version in [go.mod](go.mod)) and Docker for the local Postgres
and for the integration tests.

```bash
cp .env.example .env
make db-up      # Postgres on :5432
make build      # binary at bin/aiinv
```

## Running the tests

```bash
make test       # go test ./... -race -count=1
make lint       # go vet ./... and gofmt
make check      # both
```

One difference worth knowing: `make lint` *prints* formatting problems but
still exits 0, because `gofmt -l -d` does not signal failure. CI treats any
gofmt diff as a hard failure. Run `gofmt -w .` before pushing, or read the
`make lint` output rather than trusting its exit code.

Tests run with the race detector and `-count=1` so nothing is served from the
test cache. Integration tests spin up a real PostgreSQL via testcontainers, so
Docker must be running — they are not mocked, because the data layer is where
the interesting bugs live.

If `make` is not available (common on Windows), run the underlying commands
directly; the Makefile is thin on purpose.

## Tests come first

We write tests before implementation. Not as a ceremony — the discipline is
what keeps the service layer honest, and it is the only reason the HTTP API and
the MCP server can be guaranteed not to drift apart.

Concretely, for a change in behaviour:

1. Write a failing test that states the behaviour you want.
2. Watch it fail, for the reason you expect. A test that has never failed has
   proven nothing.
3. Write the smallest implementation that passes it.
4. Refactor with the test green.

New API surface belongs in the shared conformance specification under
`spec/conformance/`, which runs against both the HTTP API and the MCP server.
A capability that exists in one adapter but not the other is a bug.

Bug fixes need a regression test that fails before the fix.

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/), lowercase subject,
imperative mood, no trailing period:

```
<type>: <what changed>
```

Types in use: `feat`, `fix`, `docs`, `test`, `refactor`, `chore`, `ci`, `perf`.
Add a body explaining *why* when the reason is not obvious from the diff — the
diff already says what changed.

Keep commits small and self-contained. A commit that builds and passes tests on
its own is reviewable; a commit that needs the next three to make sense is not.

## Pull requests

Open an issue first for anything that changes the data model, the public API, or
the privacy boundary. For everything else a PR is fine.

Make sure `make check` passes before you push. Fill in the
[pull request template](.github/pull_request_template.md) — particularly the
privacy boundary checkbox, which is not a formality.

## The privacy boundary

AI Inventory never collects prompts, completions, conversation content,
end-user data, or model inputs and outputs. This is a hard product boundary, not
a configuration setting, and there must be no code path in any SDK that can
transmit user data. A change that would weaken this will be declined regardless
of how useful the feature is. See [SYSTEM.md §7](SYSTEM.md).

## Security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md).
