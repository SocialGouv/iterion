# ADR-139: iterion stays Go; other languages enter as sidecars behind seams

- **Status**: Accepted
- **Date**: 2026-10-27
- **Authors**: devthejo (session with Claude), operator-arbitrated
- **Code**: the seams it protects — [pkg/runtime/engine.go](../../pkg/runtime/engine.go) (`NodeExecutor`), [pkg/backend/delegate/delegate.go](../../pkg/backend/delegate/delegate.go) (`delegate.Backend`), [pkg/eventbus/bus.go](../../pkg/eventbus/bus.go) (`eventbus.Bus`); sidecar precedents: rtk (Rust output rewriter, [plugins-compression](../agents/backends/plugins-compression.md)), the agent CLIs ([ADR-065](065-dedicated-cli-agent-backend.md)/[ADR-085](085-pi-as-execution-backend.md)), [ADR-027](027-git-cli-shell-out-not-embedded-pure-go.md) (host `git`)

## Context

A proposal reached the operator: with today's coding agents able to write large
codebases quickly, migrate iterion from Go to Rust — framed alongside the
dependency-internalization proposal this ADR's sibling
([ADR-140](140-dependency-internalization-doctrine.md)) disposes of.

The measured shape of the codebase decides most of it:

- iterion is a concurrency engine. The product carries 538 `go func` sites,
  735 `select` blocks and ~7,800 `context.Context` occurrences; its shutdown
  budgets are hand-calibrated against the Go scheduler's preemption slice
  ([pkg/eventbus/bus.go](../../pkg/eventbus/bus.go)), with lossy fan-out and
  per-subscriber panic isolation as documented contracts.
- The proof mass for those contracts is CI's sharded `-race` pass over the
  whole tree plus ~3.3k test files and shared conformance suites
  (`pkg/eventbus`, `pkg/runwatch`, `pkg/credusage`). That proof verifies
  behavior **against Go's memory model and scheduler**; a port inherits none
  of it and must re-earn it against tokio's, with no differential oracle.
- Agents discount the cost of *writing* a port, not of *proving* one: the
  goldens, conformance suites and calibrated budgets are the expensive half,
  and they must be rebuilt or re-earned rather than translated.
- Velocity is the second half of the cost: ~890 commits/month on a ~7-month
  old product. A port freezes feature work or forks it across two languages
  while the DSL, backends and contracts are still moving.
- What Rust would buy here is not measurable: the workload is I/O-bound
  orchestration; the distribution constant (single static
  `CGO_ENABLED=0` binary, [ADR-019](019-sandbox-doctor-static-binary-check.md),
  [ADR-003](003-privacy-tools-pure-go.md)) is already met; memory safety is
  already met by Go; the maintenance pains actually recorded
  ([reliability audit](../reviews/reliability-audit-2026-08-31.md)) are
  distributed-systems reconciliation, independent of language.

## Decision

1. **The engine language is Go.** No port, no dual-language core.
2. **Other languages enter as sidecars behind seams**: separate processes
   holding a process-level contract (CLI arguments, line-delimited JSON on
   stdio, HTTP), with their own release and lifecycle. rtk (Rust), the agent
   CLIs and the host `git` binary are the precedents. A sidecar is an
   implementation of an existing seam (`delegate.Backend`, plugins), never a
   new engine `if` and never in-process FFI.
3. **Revisiting needs a new ADR with a proof plan**: differential goldens
   against the Go implementation, a race/invariant strategy for the target
   runtime, and a measured trigger that forced the question — not an
   anticipated one.

## Consequences

- The concurrency contracts keep their proof infrastructure; the golden and
  conformance corpus stays authoritative for both sides of future seams.
- Hiring and contribution stay on one language; the review loop
  ([adversarial rounds](../agents/workflow/adversarial-review-loop.md))
  keeps one vocabulary.
- Accepted cost: sidecars re-introduce the text-boundary work that
  [ADR-023](023-c-locale-pinned-stderr-substring-error-classification.md) and
  [ADR-024](024-nul-framed-parsing-for-adversarial-git-metadata.md) paid for
  on the git seam — each sidecar owns its framing, locale and version pins.
- The paid-knowledge asymmetry stands: seven months of calibrated contracts
  and recorded traps ([the tree](../agents/README.md)) remain assets of this
  implementation, and a port would reset them.
