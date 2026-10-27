# ADR-138: dependencies are pruned, fork-patched or kept — never rewritten wholesale

- **Status**: Accepted
- **Date**: 2026-10-27
- **Authors**: devthejo (session with Claude), operator-arbitrated
- **Code**: [scripts/deps-closure.sh](../../scripts/deps-closure.sh) (`task deps:closure`, the metric), [the ops page](../agents/ops/dependency-surface.md); sibling: [ADR-139](139-language-stays-go-sidecars-for-the-rest.md)

## Context

The operator asked whether today's coding agents make it cheaper to
internalize our Go libraries — recode them, keeping only what we use — to
shrink long-term maintenance surface and security exposure, and whether the
same effort should carry a Go→Rust migration ([ADR-139](139-language-stays-go-sidecars-for-the-rest.md)
refuses the migration).

Measurement reframed the question. The server binary's import closure links
**7,340,738 third-party LOC across 847 packages** (main `0b1de4062`), and
**68 % of that is one generality**: the modernc sqlite stack, pulled by the
codex SDK fork's *session lookup* — three exported functions iterion never
calls (grep-verified; resume flows as a JSON-RPC field resolved by the codex
binary itself, run state lives in Mongo). The genuinely attacker-facing
dependencies are tiny and battle-tested: golang-jwt 2,169 LOC,
gorilla/websocket 3,577 LOC.

On the security argument specifically: the CVE window at home is bounded —
Renovate fires security alerts outside the weekly cycle, the auto-maintenance
lane merges them with no human, govulncheck triages by reachability, and
vendor-check pins the tree byte-for-byte. Internalizing a protocol client
removes us from that advisory network: a known, hours-bounded window becomes
unknown windows with no upstream patch, in code whose only proof is our own
test suite — the fixture-gentler-than-the-attacker trap. Battle-tested code
has had its bugs found; freshly written protocol code has its bugs ahead of
it.

A 9-agent adversarial audit (ticket [#2742](https://github.com/SocialGouv/iterion/issues/2742),
2026-10-27) then priced each candidate per dependency. Its verdicts are the
evidence for this record.

## Decision

**1. Four tracks, applied per dependency with evidence, never as a blanket
rule: keep / cut / inline / fork-patch.**

**2. The internalization test.** Rewrite a dependency only when ALL hold:
high used-fraction AND small code AND no wire protocol AND weak upstream. No
current dependency qualifies. "Stricter than upstream" is exercised by
**fork + patch** (the codex SDK fork precedent: our bar ships locally while
the upstream PR sleeps), which keeps the upstream test mass and imposes ours
— a rewrite discards both.

**3. The untouchable cores** (wire-protocol or security clients, battle-tested):
mongo-driver, modelcontextprotocol/go-sdk, otel, nats.go, go-redis,
golang-jwt, gorilla/websocket, cobra, sentry-go, golang.org/x/crypto.

**4. The audit verdicts, recorded:**

| target | verdict | effect | note |
|---|---|---|---|
| codex fork sqlite stack | **cut** | ~-5.58M linked LOC | session APIs unreachable from iterion; delete the cluster in the fork, tidy+vendor; verified adversarially, `plan_holds=true` |
| tiktoken-go/tokenizer | **fork-patch** | ~-201k linked LOC | trimmed first-party fork, o200k_base only; the metric is display-only; full elimination is a product decision |
| yaml v2 stack (`go.yaml.in/yaml/v2`) | **cut** | -9,639 linked LOC | 20 mechanical importers, zero custom marshalers; migrate through `internal/yamlx` choke helpers, goldens regenerated deliberately |
| wails desktop | **decision-needed** | -81 of 561 graph modules (server LOC: 0) | alive product (ADR-134/135, release workflow); operator dossier, cut only on arbitration |
| go-selfupdate | **keep** | — | powers the desktop's signed auto-update: a real feature |
| claw fork azure/bedrock stacks | **keep** | — | reachable from iterion's model routing (verified, not assumed) |
| leaf deps (flock, cron, pty, pflag, uuid) | **keep** | — | inline churn at 52 call sites exceeds the supply-chain gain of one module |

**5. The metric is owed.** Every PR that touches go.mod cites
`task deps:closure` before/after. The number makes a regression like the
sqlite stack visible the day it re-enters.

## Consequences

- The closure number is a tracked surface, not a vanity metric: cuts land as
  PRs with before/after, and the sqlite deletion must be **re-applied on any
  future fork resync** (recorded in the fork's FORK.md) or it silently
  re-imports 5.6M lines.
- The yaml migration carries golden risk: `.bot` formatting is pinned by
  goldens; regeneration is deliberate and reviewed, never incidental.
- The **keep** verdicts are as load-bearing as the cuts: they are revisited
  with new evidence — a routing change orphaning azure/bedrock, an operator
  arbitration on desktop — not by re-running the same proposal.
- The audit's correction stands as method: measure the closure, verify the
  cut adversarially, and record the keep with the same care as the cut.
