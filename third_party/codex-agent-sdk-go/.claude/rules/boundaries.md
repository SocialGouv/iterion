# Boundaries

## Always

- Follow existing patterns in neighboring code before introducing new patterns.
- Keep behavior changes covered by tests in the same PR.
- Run relevant tests for changed code; run `go test ./...` for substantial changes.
- Keep user-facing and agent-facing docs aligned when public behavior changes.

## Ask First

- Adding exported API surface (new public functions/types/options).
- Changing transport interfaces or protocol semantics.
- Adding new third-party dependencies.
- Making breaking behavior changes to existing option semantics.

## Never

- Leave errors unchecked.
- Store `context.Context` in structs.
- Mix naming that implies another SDK/provider when describing this codebase.
- Bypass backend capability checks for options.
