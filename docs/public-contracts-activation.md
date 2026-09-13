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
`consumer_access_evidence` field is a string, not that verification. No
distributed `contracts probe` or `activate` command exists. Consequently
there is no safe positive distributed activation procedure in this change.
Do not enable native launches on a shared queue based on the local proof,
queue version, or a manually populated evidence string. Before a distributed
rollout, implement and test a trusted fleet/ACL census and epoch-fenced
admission, including old consumers, delayed messages, stale/unknown
participants, revocation and rollback. The default ordering can then be
server-first; runner-first requires a separately verified queue-compatibility
window.

This document is a compatibility/runbook snapshot, not a production rollout
approval. The complete status is tracked in
[public-contracts-acceptance.md](public-contracts-acceptance.md).
