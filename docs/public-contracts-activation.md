# Public-contract compatibility and activation for #1165

`ports-v1` changes execution semantics; `dsl: 2` changes only the source
syntax profile. A legacy `.bot` keeps its control-flow engine unless it
explicitly declares `runtime_semantics: "ports-v1"`, a public contract and a
data graph. Parsing a legacy workflow or producing an incomplete conversion
draft never changes its interpreter.

| Boundary | Legacy | Native `ports-v1` | Compatibility rule |
| --- | --- | --- | --- |
| Execution | Ordered control edges and existing fan-out/loops | Committed typed port suppliers and ready-node scheduling | An existing run cannot switch semantics, including forced resume or fork. |
| Run ID and records | Ordinary ID under `runs/` or legacy Mongo collection | Reserved `pc1_` ID under `port_runs_v1/` or native Mongo collection | Supported older list/delete/prune/repair paths cannot address the native closure. |
| Blobs and files | Existing key families | `ports-v1/` blob prefix and immutable output captures | Old blind blob deletion cannot sweep the native family. |
| Queue | Legacy launches emit v14 | Native launches require v15 plus explicit semantics and reserved ID | New runners accept supported old envelopes; v14 runners reject v15 before Store access. Rejection alone does not guarantee delivery. |
| Activation | No native gate | Exact store identity, current binary capability, expiring proof and immutable run admission | Rollback disables new launches; an already admitted compatible run can resume. |

## Local private-store procedure

Use a dedicated store directory whose writers and automation have been
inspected. The `--exclusive-store` flag is an operator attestation that no
incompatible automation or old maintenance process can access it; directory
permissions alone do not prove that fact. `probe` creates a new private local
store if the path does not yet exist. `inspect` and `activate` require an
existing store.

```bash
iterion contracts --store-dir /path/to/private-store probe --exclusive-store > /path/to/proof.json
iterion contracts --store-dir /path/to/private-store inspect --json
iterion contracts --store-dir /path/to/private-store activate --proof /path/to/proof.json
```

Review the proof's scope, canonical store identity, capability digest, queue
version and expiry before activating. Activation recomputes these facts and
refuses a changed or expired proof. Proofs expire within 24 hours. After a
binary or store change, probe again; do not copy a proof or a run to another
store. The local suite covers a fresh private store, stale/changed proof,
copied-store refusal, rollback, and an admitted run continuing afterward.

To stop new native launches while preserving already admitted runs:

```bash
iterion contracts --store-dir /path/to/private-store deactivate
iterion contracts --store-dir /path/to/private-store inspect --json
```

Keep a compatible binary and its native storage namespace available until
those admitted runs finish or are deliberately resolved. Deactivation is a
revisioned storage write, so it does not erase their admission records.

## Rewind an admitted native run

An explicitly selected node can be rewound in a stopped, resumable native
run. The node and its data descendants are invalidated together; independent
committed work and the consumed budget remain. This also applies to every
item of a mapped node, including an empty collection. Captured supplier
revisions retain dependencies that an edited source may have removed.

```bash
iterion rewind --run-id pc1_RUN --node render --restore-scope none
iterion resume --run-id pc1_RUN
```

Use `resume --force` when deliberately resuming edited source. A rewind
cannot change the run's runtime semantics or erase unresolved effects.
Native rewind currently keeps workspace files; immutable output captures
lose their publication references and cannot satisfy the rerun. Explicit
workspace restoration and automatic pivot selection are unavailable; a
worktree run requires `--restore-scope none`. External effects already
performed are not undone.

## Distributed activation remains blocked

The current chart defaults to the shared JetStream `$G` account and the
`iterion-runners` durable consumer. A v14 runner can fetch a v15 native
message, delay-Nak it repeatedly and eventually park it after its delivery
budget is exhausted. Therefore the v15 rejection test proves that an old
runner will not execute native semantics; it does **not** prove that a native
run will reach a capable runner.

The repository has no trusted, complete inventory of every consumer
authorized to fetch that durable, no freshness-bound participant evidence,
and no reconciliation against queue-account permissions. The stored
`consumer_access_evidence` field is a string, not that verification. The
production Mongo Store supplies no distributed-access verifier: even a
manually written, otherwise valid activation record now fails the launch
gate. Distributed evidence is limited to a 60-second validity window and a
future-dated proof is refused. Mongo Engine tests inject a separate verifier
for their isolated fixture; that fixture does not certify a deployment.

The selected authority for a future positive proof is the NATS system
account together with the Kubernetes API. A live NATS connection census must
be reconciled with the complete set of workloads that can use the durable,
including dormant ReplicaSets and scaled-to-zero runners. Because a live
connection list says nothing about who may connect later, the proof must also
establish effective NATS subject permissions for
`$JS.API.CONSUMER.MSG.NEXT.<stream>.<consumer>` and the credentials available
to each workload. Unknown external clients, unrestricted shared credentials,
unreadable ACLs or incomplete Kubernetes list permissions fail the probe.
The activation epoch and access inventory must be rechecked before expiry;
revocation must stop new launches. No distributed `contracts probe` or
`activate` command exists yet, so there is no safe positive distributed
activation procedure in this change. The default ordering can then be
server-first; runner-first requires a separately verified queue-compatibility
window.

This document is a compatibility/runbook snapshot, not a production rollout
approval. The complete status is tracked in
[public-contracts-acceptance.md](public-contracts-acceptance.md).
