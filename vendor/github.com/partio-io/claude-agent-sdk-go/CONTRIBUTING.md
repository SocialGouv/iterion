# Contributing

We welcome contributions to the Claude Agent SDK for Go.

## Getting Started

1. Fork the repository
2. Create a feature branch: `git checkout -b my-feature`
3. Make your changes
4. Run checks: `make lint test`
5. Commit and push
6. Open a pull request

## Guidelines

- **Zero external dependencies.** The SDK uses only the Go standard library. Do not add third-party modules.
- **Standard library testing.** Use `testing` only — no testify, gomock, or other test frameworks.
- **Table-driven tests.** Prefer table-driven tests with sub-tests (`t.Run`).
- **One concern per file.** Each file should have a single, clear responsibility.
- **Run `make lint test`** before submitting. CI will verify this.

## Code Style

- Follow standard Go conventions (`gofmt`, `goimports`).
- Use `go vet` and `golangci-lint` to catch issues.
- Keep functions focused and small.

## Pull Requests

- Keep PRs focused on a single change.
- Include tests for new functionality.
- Update documentation if behavior changes.
