# Contributing to Levee

Thanks for your interest. Levee is a young project with a deliberately narrow
scope, and contributions that respect that scope are very welcome.

## Scope

Levee does budget enforcement for AI agent traffic. Features that do not
directly serve budget enforcement (routing, caching, prompt tooling,
credential management) are out of scope for now. Improving existing behavior
takes priority over adding new behavior. If you have an out-of-scope idea,
open an issue and it will be labeled phase-2 rather than rejected.

## Before you start

- Bug fixes: a pull request is welcome directly. Include a test that fails
  without the fix.
- Features and behavior changes: open an issue first and wait for agreement.
  This protects your time. A feature PR without a prior issue may be closed.

## Development setup

- Go 1.26 or later.
- golangci-lint v2.12.2 (the version CI pins, v1 configs are incompatible).
- Clone, then verify your environment: `make build && make test`.

## The gate

Every pull request must pass the same commands CI runs:

    make lint
    make test
    make bench
    make build

Benchmarks run on every pull request. Optimizations must include before and
after benchmark results in the PR description. An automated benchmark
regression gate is planned but not yet enforced by CI.

## Pull requests

- One focused change per PR. Small is good.
- Tests at the boundary: integration-style tests against real request and
  response shapes are preferred over mock-heavy unit tests.
- Conventional commit messages (feat:, fix:, docs:, test:, chore:).
- PRs are squash merged by the maintainer, no need to squash locally.

## Bug reports

Include your config (redact anything sensitive), the Levee version or commit,
and reproduction steps. For budget accounting bugs, include the request,
response, and the budget state you expected versus observed.

## Conduct and security

See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) and [SECURITY.md](SECURITY.md).
