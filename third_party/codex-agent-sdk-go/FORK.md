# Local fork of github.com/ethpandaops/codex-agent-sdk-go

Vendored copy of upstream **v0.0.13** (latest release at the time of the fork,
2026-07-16), wired in through a `replace` directive in the root `go.mod`.

## Why this fork exists

Codex app-server image-generation events (`image_generation_end`) inline the
generated image as base64 **in a single JSON-RPC line** (~3.4 MB for a
1024px PNG). The SDK's stdout readers were capped at 1 MB per line and never
checked the scanner error, so the session died silently on every image
generation: no result message, no error — the caller only saw
`delegate: codex: no result after 3 attempts`. Upstream `master` still has
the bug; drop this fork and restore the upstream requirement once it is
fixed there.

Upstream tracking: issues
[ethpandaops/codex-agent-sdk-go#22](https://github.com/ethpandaops/codex-agent-sdk-go/issues/22)
(silent death on multi-MB lines) and
[#23](https://github.com/ethpandaops/codex-agent-sdk-go/issues/23)
(`local_image` rejected), fixed by PR
[#24](https://github.com/ethpandaops/codex-agent-sdk-go/pull/24) — the same
patches as this fork. Once #24 is merged and released: delete
`third_party/`, drop the `replace`, bump the requirement.

## Local patches on top of v0.0.13

1. `internal/subprocess/appserver.go` — scanner line cap raised 1 MB → 64 MB
   (initial allocation stays 1 MB); `readLoop` records the scanner error via
   `recordReadErr()` (exposed as `ReadErr()`), except during a graceful
   `Close`.
2. `internal/subprocess/appserver_adapter.go` — `surfaceReadErr()` forwards a
   fatal transport read error to the `errs` channel so the query loop reports
   it instead of ending silently without a result.
3. `internal/subprocess/cli.go` — same 64 MB cap for the exec transport;
   initial buffer allocation made lazy (was eagerly allocating the full cap).
4. `internal/protocol/protocol.go` — the controller drains a pending
   transport error when the messages channel closes first (the `select`
   between two ready channels is random, losing the error ~50% of the time),
   and keeps draining buffered messages when the errs channel closes first
   (symmetric interleaving that dropped late events, e.g. `turn.completed`).
5. `internal/message/content.go` — `InputLocalImageBlock.MarshalJSON` emits
   the `localImage` discriminator. The live codex app-server rejects the old
   `local_image` spelling with `unknown variant `local_image`, expected one
   of `text`, `image`, `localImage`, `skill`, `mention``; it remains accepted
   on input for backward compatibility.

Regression tests added: `internal/subprocess/appserver_bigline_test.go`
(fake app-server emitting a 3 MB notification line) and
`internal/protocol/fatal_error_drain_test.go` (both shutdown interleavings).
