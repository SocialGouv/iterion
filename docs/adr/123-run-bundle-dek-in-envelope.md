# ADR-123: Run bundles seal under a per-run DEK carried in the queue envelope

Date: 2026-10-06. Status: accepted (operator decision, sovereign pools P4-b / D13).

## Context

A run's pre-resolved credentials (BYOK keys, OAuth payloads, generic secrets) travel as a
sealed `RunBundle` the publisher writes to `run_secrets` and the runner opens just before
executing. Since the key-ring slice (P4-a), the bundle carries a key id and binds to
tenant/pool/run, but the RING that seals it sits in the process environment of both the
server and every runner pod. A runner pod is inside the runtime's trust domain — the
admission's own comment puts pod secrets "one /proc/self/environ read away" from an agent —
so the ring on a pod is the ring for EVERY tenant: past bundles, in-flight bundles, and
(via the ring's open fallback) at-rest credentials. D13's residual: the publisher alone
should hold the keys.

Two shapes were considered; both were pre-declared compatible with the bootstrap/runtime
split (the runner wires its sealer at boot, runs consume per-message).

## Options

**A — Per-run DEK in the envelope (chosen).** The publisher generates a fresh 32-byte DEK
per run, seals the bundle under it (the same tenant/pool/run AAD), and puts the DEK on the
RunMessage (schema v24). The DEK transits only the run's own pool stream (subject-split)
and the server that published it. Runner pods hold NO platform key material: the per-run
sealer is built from the envelope DEK at claim time. Compromising a pod yields the DEKs of
that pod's own in-flight claims — and by the one-team-per-pool invariant those are the
team's own runs. The ring retreats to the server, where it still seals at-rest records.

**B — Mediated refresh (rejected).** Pods request unsealing over an authenticated channel.
Same isolation gain, but a new RPC surface, per-claim latency, an availability coupling
(server down = no run starts), and a decryption oracle in the data path — more surface for
the same boundary.

## Decision

Option A. `RunMessage.BundleDEK` rides schema v24; records stamp `key_id: "dek"` for the
scheme, and the two older cohorts keep opening: id-less records (pre-P4a, run-only AAD)
and ring-id records (P4a, extended AAD) through the optionally-wired runner ring, which
exists only for the transition window — the 24 h bundle TTL clears it, after which the
runner deployment drops the auth secret entirely (the chart documents the step). Rotation
of the ring becomes a server-only concern (at-rest records); run bundles stopped using it.

## Consequences

- The runner's attack surface loses the platform's key material entirely; a pod compromise
  is bounded by the one-team-per-pool invariant instead of the fleet.
- The DEK is plaintext in the envelope: the boundary it relies on is the pool stream split
  (P1b-iii) plus the one-team-per-pool invariant (P4-c). Breaking either re-opens this —
  which is the honest statement of what D13 always rested on.
- A DEK-less message claiming `key_id: "dek"` fails closed with a named error.
- Redelivery and DLQ replay re-publish the message verbatim: blob + DEK travel together,
  no server-side re-seal, no new recovery path.
- Deploy ordering stays runners-before-servers: a server publishing v24 is refused by
  pre-v24 runners at the schema check (explicit, not silent).
