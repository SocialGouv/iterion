# Build and Test

## Default Commands

```bash
go build ./...
go test ./...
go test -race ./...
go test -v -run TestQuery ./...
go test -tags=integration ./integration/...
golangci-lint run
go run ./examples/quick_start
```

## Command Usage

- Use targeted tests first while iterating (single package or `-run` pattern).
- Before finishing substantial code changes, run `go test ./...`.
- Run `go test -race ./...` for concurrency-sensitive changes.
- Run `golangci-lint run` before finalizing when Go files changed.

## Integration Test Notes

- Integration tests require Codex CLI availability and a working runtime setup.
- If integration tests are not runnable in the current environment, call that out explicitly.
