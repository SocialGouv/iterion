[← Security bots](security-bots.md)

# Distributed security scans — design (Cap. 3)

> **Status: V1 foundations shipped (local-mode primitive + runtime
> fields + tests). Bundle integration + cloud-mode path pending.**
> See "Status & shipped pieces" at the bottom. Targeted for parity
> with deepsec's `pnpm deepsec sandbox process --sandboxes N
> --concurrency M`.

## Motivation

`sec-audit-source` V1 is single-process: one `iterion run` reads every
file, runs scanners, triages, revalidates. For repos > 100k LOC this
takes hours and easily $100+ in revalidate tokens. deepsec solves
this by sharding the file list across N worker pods, each doing a
slice. We want the same shape.

The Cap. 1 FileRecords work is the prerequisite: with per-file
append-only records, each shard can write its own without
coordinating — the parent (or a subsequent run) aggregates.

## Constraints

1. **Single-machine path must work**. Not every user has a NATS
   cluster. The same bundle should fan out to N subprocesses on
   one host when no queue is configured, and to N runner pods when
   a queue is configured. Same DSL, same outcomes — only the
   data plane differs.

2. **No new top-level DSL primitives.** Sharding is a property of
   the security bundle, not the language. Wire it through a
   hidden CLI subcommand + bundled `tool` nodes.

3. **Polling-based completion**. No new NATS results topic in V1.
   The parent polls the run store for each child until terminal
   status. Adds latency (poll interval) but avoids a new data
   path. Future iteration may add an event topic.

## Data model

### `RunMessage` extensions (`pkg/queue/types.go`)

```go
type RunMessage struct {
    // ... existing fields ...

    // Cap. 3 — distributed security scans
    ParentRunID  string `json:"parent_run_id,omitempty"` // empty = root run
    ShardIndex   int    `json:"shard_index,omitempty"`   // 0..ShardCount-1
    ShardCount   int    `json:"shard_count,omitempty"`   // total shards
    ShardLabel   string `json:"shard_label,omitempty"`   // human-friendly tag for UI
}
```

Adding fields is backward-compatible: existing publishers/consumers
ignore unknown fields. No migration needed.

### `run.json` extensions (`pkg/store/store.go`)

```go
type Run struct {
    // ... existing fields ...

    ParentRunID string `json:"parent_run_id,omitempty"`
    ShardIndex  int    `json:"shard_index,omitempty"`
    ShardCount  int    `json:"shard_count,omitempty"`
}
```

Allows the studio + `iterion inspect` to surface "this run was
shard 3/8 of <parent>". Cheap to add.

## CLI surface

One new hidden subcommand: `iterion __scan-shards`.

```
iterion __scan-shards \
    --parent-run-id=<id> \
    --workflow=<path-to-bundle> \
    --files-json=<path-or-stdin> \
    --shard-size=<files-per-shard> \
    --base-vars-json=<json-blob> \
    --store-dir=<dir> \
    --max-concurrency=<n> \
    [--mode=auto|cloud|local] \
    [--poll-interval=2s] \
    [--timeout=2h]
```

Behavior:

1. **Plan**. Read `files-json`, split into `ceil(len/shard_size)`
   shards. Each shard gets a deterministic `shard_id` derived from
   `sha256(parent_run_id + shard_index)` (so a re-run of the parent
   produces the same child IDs).

2. **Submit**.
   - In `cloud` mode (NATS available): publish N `RunMessage`s with
     `ParentRunID` set, `Vars` including the shard's file list
     under a well-known key (`security_shard_files`).
   - In `local` mode: fork N subprocesses
     (`iterion run <workflow> --var security_shard_files=... --parent-run-id=...`),
     respecting `--max-concurrency`.
   - In `auto` mode: cloud if `ITERION_QUEUE_NATS_URL` is set, else
     local.

3. **Wait**. Poll the run store every `poll-interval` for each
   child's status. Collect terminal statuses. Fail-fast on
   `failed` (unless `--continue-on-failure`).

4. **Aggregate**. Emit a JSON envelope:
   ```json
   {
     "parent_run_id": "...",
     "shard_count": 8,
     "shards": [
       {"shard_index": 0, "run_id": "...", "status": "finished",
        "files_audited": [...], "issues_created": [...]},
       ...
     ],
     "merged_issues_created": [...],
     "errors": []
   }
   ```

5. **Cleanup**. Child runs remain in the store for audit; their
   FileRecords are already merged into the parent's
   `.iterion/security/files/` (cross-run, append-only).

## Bundle integration

`sec-audit-source/main.bot` grows two new nodes between
`scan_join` and `triage`, gated by `--var shard_size=<N>` (default
`0` = no sharding):

```iter
compute plan_shards:
  ## Lists every source file the scanners touched. If shard_size > 0,
  ## emits a shards plan; otherwise emits a single-shard plan (no fan-out).
  input:  plan_input
  output: plan_output
  expr:
    enabled: "vars.shard_size > 0"
    ...

tool dispatch_shards:
  ## Calls `iterion __scan-shards`. Skipped when plan_shards.enabled is false.
  command: `iterion __scan-shards --parent-run-id={{run.id}} ...`
  ...

# Edges:
scan_join -> plan_shards
plan_shards -> dispatch_shards when enabled
plan_shards -> triage when not enabled
dispatch_shards -> done  # children emit their own issues + FileRecords
```

When sharding is enabled, the parent doesn't run triage / revalidate
/ report_card itself — the children do, each on their slice. The
parent's role is just to plan + dispatch + collect.

When sharding is disabled (default), the existing single-process
pipeline runs unchanged.

## Cloud path

Required infra:
- NATS JetStream cluster (already used by iterion cloud mode)
- Runner pods (already supported by `pkg/runner/`)
- KEDA scaler on queue depth (already wired in `charts/iterion/`)

The change is **only** the `RunMessage` field additions and the
parent-aware execution path in the runner. Runner logic:

```go
// pkg/runner/loop.go, after the run finishes:
if msg.ParentRunID != "" {
    // No new emission needed in V1 — parent polls the store.
    // V2 can publish an event to a per-parent results topic.
}
```

## Local path

`iterion __scan-shards --mode=local` spawns N subprocesses sharing
the same `--store-dir`. Each subprocess is a normal `iterion run`,
just with `--var security_shard_files=...` and `--parent-run-id=...`.

Concurrency is bounded by `--max-concurrency` (default
`runtime.NumCPU() / 2`).

This path is fully functional without NATS — useful for
development, single-machine large-repo audits, and CI runners that
can dedicate one big box to a scan.

## Tests

1. **Unit**: `pkg/queue/types_test.go` — extended `RunMessage`
   round-trips.
2. **Unit**: `pkg/runner/loop_test.go` — runner records
   `parent_run_id` on the run if present.
3. **Integration (local-mode)**: e2e test spawns 4 shards via
   `iterion __scan-shards --mode=local` on a fixture workspace,
   asserts 4 child runs reached terminal status, 4 FileRecord
   files were written.
4. **Integration (cloud-mode)**: gated on `ITERION_QUEUE_NATS_URL`
   set; skipped in normal CI.

## Open questions

- **Children writing to the parent's FileRecords directory** — V1
  has them all write to `<workspace>/.iterion/security/files/`. Two
  children scanning overlapping files would race. Decided: shard
  boundaries are by file path, so each child writes a disjoint set.
  Future: a child computes its filename hash range and only writes
  files in that range.
- **Children creating board issues** — V1 has each child call
  `mcp__iterion_board__create_issue`. Two children scanning
  overlapping files would create duplicate issues. Same mitigation:
  shard by path is disjoint by construction.
- **Run lifecycle UI** — the studio should show the parent run with
  N children as a tree. `pkg/server` + `pkg/runview` extensions to
  surface this. **Out of scope for V1**.

## Implementation order

1. ✅ Cap. 1 FileRecords (done — see this bundle's V1.1).
2. Runtime: `RunMessage` + `Run` field additions, runner records
   them. Unit tests.
3. CLI: `iterion __scan-shards` (local mode only first). Test
   harness.
4. Bundle: `plan_shards` + `dispatch_shards` nodes. e2e test
   (local mode).
5. CLI: cloud-mode publishing path in `__scan-shards`. Integration
   test gated on NATS.
6. Studio: parent-with-children tree view. (Stretch — V2.)

Total estimate: 3-5 days for steps 2-5 (step 6 deferred).

## Status & shipped pieces (as of V1.1)

✅ Step 1 — Cap. 1 FileRecords (V1.1, see this bundle's
[skills/file-records.md](../examples/sec-audit-source/skills/file-records.md)).

✅ Step 2 — Runtime field additions:
- `pkg/queue/types.go` — `RunMessage.ParentRunID`, `ShardIndex`,
  `ShardCount`, `ShardLabel` (back-compat optional fields).
- `pkg/store/run.go` — same four fields on `store.Run` so they
  persist + surface in the studio.
- Tests in `pkg/queue/` + `pkg/store/` pass unchanged.

✅ Step 3 — CLI primitive — `iterion __scan-shards` (local mode)
implemented at `cmd/iterion/scan_shards.go`. Deterministic shard
ids (`sha256(parent_run_id || index)`), bounded concurrency via
semaphore, child subprocess spawn, store polling on completion,
aggregated JSON envelope on stdout. Unit tests
(`cmd/iterion/scan_shards_test.go`): plan determinism,
disjoint-parent isolation, remainder shard, id format, empty
input.

⏳ Step 4 — Bundle integration: requires the scanner tool nodes
in `sec-audit-source/main.bot` to accept a `--var file_filter`
(comma-separated paths) so a child run scans only its slice. The
scanners (`semgrep`, `gosec`, `bandit`, `gitleaks`, `trivy`)
each support `--include` / explicit file args, so the change is
in the command templates rather than the runtime. **Pending.**

⏳ Step 5 — Cloud-mode publishing: swap the local
`exec.CommandContext` in `dispatchLocal` for a NATS JetStream
publish via `pkg/server/cloudpublisher`. The parent run id +
shard fields go into the published `RunMessage`; the existing
runner pool drains the queue with `MaxAckPending=1`. No new
NATS topic, no event-driven aggregation — the parent polls the
store. **Pending.**

⏳ Step 6 — Studio parent/child tree view. **Deferred to V2.**

## How to use the primitive today (manual integration)

Until step 4 lands, an operator can invoke `iterion __scan-shards`
directly from a tool node (or from a wrapper script):

```bash
# Build a JSON array of files to shard:
find . -type f -name '*.go' | jq -R . | jq -s . > /tmp/files.json

# Fan out across 4 child runs, 50 files per shard:
iterion __scan-shards \
  --parent-run-id="$(uuidgen)" \
  --workflow=examples/sec-audit-source/main.bot \
  --files-json=/tmp/files.json \
  --shard-size=50 \
  --max-concurrency=4 \
  --store-dir=$(pwd)/.iterion \
  --base-vars-json='{"workspace_dir":"'"$(pwd)"'"}' \
  --shard-var=security_shard_files \
  | jq .
```

The JSON envelope on stdout reports each child's terminal status
+ paths, and the JSON's `errors[]` is non-empty when a shard
failed.

In a sandboxed run (when `host_state: auto` is in effect), the
iterion binary is bind-mounted into the container, so
`__scan-shards` resolves to the same binary and the children run
inside the same sandbox.
