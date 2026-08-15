## What this changes

<!-- One or two sentences. What does this do, and why now? -->

## Why

<!-- The reasoning the diff cannot show. Link the issue if there is one. -->

Closes #

## How it was tested

<!-- What you ran, and what a reviewer should run to see it work. -->

## Checklist

- [ ] Commits are signed off (`git commit -s`) — see [CONTRIBUTING.md](../CONTRIBUTING.md)
- [ ] `make check` passes locally (`go vet`, `gofmt`, `go test ./... -race`)
- [ ] Tests were written before the implementation, and I watched them fail first
- [ ] New API surface is covered by the shared conformance spec in `spec/conformance/`, and works identically over HTTP and MCP
- [ ] Commit messages follow Conventional Commits

## Boundaries

- [ ] **Privacy boundary intact** — this change adds no code path that could collect prompts, completions, conversation content, end-user data, or model inputs and outputs ([SYSTEM.md §7](../SYSTEM.md))
- [ ] **License boundary intact** — no code moved between `sdk/` (Apache-2.0) and the rest of the repository (AGPL-3.0)
- [ ] Records are flagged rather than rejected, and nothing is deleted automatically ([SYSTEM.md §6](../SYSTEM.md))

## Anything else

<!-- Migrations, config or env var changes, follow-up work, known gaps. -->
