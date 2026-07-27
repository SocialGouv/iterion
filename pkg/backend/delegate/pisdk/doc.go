// Package pisdk is a Go port of the client surface pi (https://pi.dev,
// github.com/earendil-works/pi) publishes for driving its coding agent from
// another process.
//
// It is the pi counterpart of pkg/backend/delegate/claudesdk: a transport +
// wire-type layer with no iterion semantics, so it can be exercised on its
// own against a scripted fake `pi`.
//
// # Why a port rather than ad-hoc decoding
//
// pi exports RpcClient and the whole RPC type set from its package entry
// point (packages/coding-agent/src/index.ts), and specifies the protocol in
// packages/coding-agent/docs/rpc.md. Those files encode semantics that are
// not guessable from the outside and that are expensive to rediscover by
// trial and error — notably:
//
//   - the response to `prompt` fires at PREFLIGHT, not at completion; the
//     real completion boundary is the `agent_settled` event;
//   - commands are dispatched without serialisation, so responses can arrive
//     out of order and MUST be correlated by id;
//   - pi gates its own stdout on reader backpressure, so a stalled consumer
//     stalls the agent itself;
//   - closing stdin is the graceful-shutdown signal;
//   - framing is LF-only: splitting on the Unicode line separators that
//     Node's readline also breaks on would corrupt JSON string payloads.
//
// # Provenance
//
// Ported from pi at commit a597371b (v0.82.1-22-ga597371b), from:
//
//	packages/coding-agent/src/modes/rpc/rpc-types.ts     → rpc.go
//	packages/coding-agent/src/modes/rpc/jsonl.ts         → jsonl.go
//	packages/coding-agent/src/core/agent-session.ts      → event.go
//	packages/coding-agent/src/core/session-manager.ts    → event.go (SessionHeader)
//	packages/agent/src/types.ts                          → event.go (AgentEvent)
//	packages/ai/src/types.ts                             → message.go
//	packages/ai/src/utils/diagnostics.ts                 → message.go (Diagnostic)
//
// When bumping the pinned pi version, re-read those files and update this
// list. Fields iterion does not consume are still declared, because their
// absence is how a protocol change goes unnoticed.
//
// # Decoding posture
//
// Every event and message type decodes leniently: unknown `type` values are
// preserved as-is rather than rejected, and optional fields are pointers or
// zero-valued. pi ships roughly weekly on a 0.x line, so a new event variant
// must be a no-op for iterion, never a parse failure.
package pisdk
