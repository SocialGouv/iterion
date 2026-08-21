# ADR-089 — `session: persist`: a node resumes its own conversation on re-entry

- **Status**: Accepted (revision 8 — Codex review)
- **Date**: 2026-08-21
- **Supersedes**: nothing
- **Extends**: ADR-060, ADR-058, ADR-087, ADR-073
- **Non-goal**: inner judge/human oracles inside an agent node; claw
  transcript rehydration (reserved `conversation_ref`, not this PR)

## Revision 8 (what changed)

v7 leftover: a literal `with { _session_id: "…" }` is still "produced"
by edge evaluation, so it could pass "exactly one producing edge"
while the ADR also requires literals to be refused. Provenance is now
the **source output stamps**, not the mapping result.

| # | Review finding | Decision |
|---|---|---|
| 1 | Literal `with` values count as edge-produced | Provenance = (1) edge delivered this final id **and** (2) `rs.outputs[src]["_session_id"] == id` **and** (3) `rs.outputs[src]["_backend"] == capabilityBackend`. Exactly one such `src`, else fresh. Then slot unpack or `HasSession`. |
| 2 | HasSession fingerprint from inbound | After a visit-1 match, fingerprint comes from `rs.outputs[src]["_session_fingerprint"]` on the HasSession path; **slot.Fingerprint** still wins when a slot exists. |

v7 items that stand: PauseRun never Deletes; Get/unpack strips keys;
fail-stop durable new slot; no ref on outputs; whitelist; cloud twin;
C176; fan-out.

## Context

Film/shorts writer → judge → tool-oracle → back to the **same** writer
today starts a new conversation every refusal (`session: fresh`, often a
sibling `correct_*` node).

What the engine actually does today (re-verified):

- `applySessionContinuity` copies any non-empty `_session_id` onto
  `Task.SessionID` with no file check.
- Cloud runner deletes OAuth tempdirs on every `executeRun` return,
  pause included.
- `Execute` interaction path: `return nil, &ErrNeedsInteraction{…}` —
  no output map.
- `PauseRun` / `SaveCheckpoint` are single Mongo `UpdateOne`s; a
  client error does **not** mean the write was rolled back
  (`pkg/store/mongo/runs.go`).
- Rewound runs → `resumeFromFailure`.
- `failRunWithCheckpoint` tries `FailRunResumable`, then falls back
  to terminal `failRun` if that write fails (`run_failure.go`).
- Loop snapshots (`LoopPreviousOutput`) and fork `copyOutputs` keep
  node outputs after the node has moved on.

Kind-based "keep open until a judge approves" remains rejected.

## Decision

Sixth session mode, **appended** to the iota. Update
`pkg/dsl/ast/jsonenc.go`.

```text
session: persist
```

Judges/humans stay graph nodes. `fresh` stays the default.

### Two durable refs (do not conflate)

| Ref | Lives on | Means |
|---|---|---|
| `NodeSessions[node].StateRef` | `Checkpoint.NodeSessions` | Last **completed** persist visit |
| `Checkpoint.BackendSessionStateRef` | pause checkpoint | In-flight CLI `ask_user` |

Never unpack `NodeSessions[node]` for an in-flight `ask_user`.
Claw mid-node stays on `BackendConversation`.

**Do not** put StateRef strings on node outputs, artifacts, events,
or loop snapshots. Outputs outlive blobs (loop history, fork
`copyOutputs`). Visit-1 inherit reads `rs.nodeSessions`, not output
maps.

### `ask_user` transport

```go
type ErrNeedsInteraction struct {
    // existing: NodeID, Questions, SessionID, Backend,
    // Conversation, PendingToolUseID

    SessionStateBlob []byte  // packed CLI session; transient
    SessionStateRef  string  // set only when reconstructing from checkpoint
}
```

Executor packs **before** `return nil, err` (config dir still live).
Never log or JSON-encode `SessionStateBlob`. Never put it on
`result.Output`.

Runtime:

1. `Put(A)` → `pauseInfo.BackendSessionStateRef = A`; drop the slice.
2. Put fail: pause without a CLI state ref (do not block `ask_user`).
3. `PauseRun(checkpoint with A)`.

**On any PauseRun error: keep the new blob. Do not LoadRun. Do not
Delete.** A client timeout can land after a successful Mongo
`UpdateOne`; a subsequent read is not a linearization proof. Orphans
are `DeleteRun`'s job.

Resume reconstruct: `ni.SessionStateRef = cp.BackendSessionStateRef`;
blob nil; `reInvokeBackend` Gets + unpacks via `_session_state`.
Get/unpack failure: **strip** `_session_id` and
`_session_fingerprint`, do not set `Task.SessionID`, warn + fresh
(same rule as a completed-slot Get/unpack failure).

### Pause-ref lifecycle

| Event | Sequence |
|---|---|
| First `ask_user` | Pack → `Put(A)` → `PauseRun(A)`. PauseRun **error** ⇒ **keep A**. Never Delete on that error. |
| Second `ask_user` | Pack → `Put(B)` → `PauseRun(B)`. **Success:** `Delete(A)`. **PauseRun error:** keep **A and B**. Never Delete on that error. |
| `reInvoke` succeeds, **persist** | `Put(C)` → memory slot C → required checkpoint (slot C, **clear** pause ref) → `Delete(B)` and previous completed blob. Checkpoint fail: fail-stop, keep C in memory, keep blobs, **do not** restore old slot. |
| `reInvoke` succeeds, **not persist** | Checkpoint **clears** pause ref only. **No** `NodeSessions` entry. Then `Delete` pause blob. |
| Rewind | Checkpoint without those refs **first**, then `Delete` blobs. |

### Completed-visit slot

```go
type NodeSessionSlot struct {
    Backend         string `json:"backend"`
    SessionID       string `json:"session_id,omitempty"`
    Fingerprint     string `json:"fingerprint,omitempty"`
    StateRef        string `json:"state_ref,omitempty"`
    ConversationRef string `json:"conversation_ref,omitempty"`
}
```

Immutable refs (`<node>/<ulid>`, pause `<node>/pause/<ulid>`).
Empty `StateRef` ⇒ not a slot. Never `--resume` an id without a
successful unpack or `HasSession`.

**No `_session_state_ref` on output.**

### Slot commit — fail-stop

After a finished persist visit:

1. Pack → `Put(newRef)`.
2. Memory ← new slot.
3. Required `SaveCheckpoint`. On failure: fail-stop before
   `selectEdge`. Memory stays on the **new** slot. Both blobs kept.
   `FailRunResumable` must be invoked with a checkpoint that
   **contains the new slot** (so a later Resume is visit-N of that
   conversation, not visit-N-1). Never write the old slot. If
   `FailRunResumable` itself fails, fall through to terminal failure
   **still without restoring the old slot**; blobs remain for GC.
4. Only after a successful *session* checkpoint: best-effort
   `Delete` the previous completed-visit blob.

Pack/Put fail: invalidate memory slot; required checkpoint of
deletion; on that failure fail-stop with empty slot; never restore
old; blobs kept.

### `BackendSessionStore`

fs + S3/mongo twin, Get before Execute, `DeleteRun` sweeps, both
backends in `storetest`. Nil store ⇒ warn + fresh.

### Packer interface

```go
type SessionPacker interface {
    PackSession(ctx context.Context, sessionID string) (blob []byte, err error)
    UnpackSession(ctx context.Context, sessionID string, blob []byte) error
    // HasSession reports whether sessionID exists in the *live*
    // cwd / CLAUDE_CONFIG_DIR / CODEX_HOME / pi StateDir of this
    // process. Runtime must not probe those directories itself.
    HasSession(ctx context.Context, sessionID string) bool
}
```

Wired on the backends that v1 supports (`claude_code`, `pi`, `codex`).
`HasSession` uses the same root the next `Execute` will use.

Security (v4): whitelist session files only; never OAuth
(`.credentials.json`, `auth.json`); no `..`/absolute/symlink/hardlink/
special; size+count caps; no truncate; header `{backend, session_id}`;
temp extract + atomic rename; auth-file test.

### Reserved keys (success path only)

| Constant | Value | When |
|---|---|---|
| `SessionStateKey` | `_session_state` | input: blob to unpack |
| `SessionStateBlobKey` | `_session_state_blob` | output of a **successful** Execute (stripped before `rs.outputs`) |

**No** `_session_state_ref` key. `takeSessionStateBlob` before both
success-path `rs.outputs` assignments. Also `sanitizeOutputForEvent`.
ask_user path: blob rides `ErrNeedsInteraction`, not these keys.

### Inbound visit-1 seed (fail-closed, provenance from source stamps)

`HasSession` confirms **physical** presence in the live config dir. It
does **not** prove this graph emitted the id. Edge evaluation of a
literal `with { _session_id: "sess-other" }` "produces" that id — that
is **not** provenance. Provenance is the **source node's output stamps**.

Let `id` be inbound `_session_id`. A source `src` is **eligible** iff
**all** of:

1. An incoming edge from `src` delivered this final `id` to input
   (the mapping result equals `id`).
2. `rs.outputs[src]["_session_id"] == id` (the source actually
   served that session; a literal in `with {}` fails here).
3. `rs.outputs[src]["_backend"] == capabilityBackend`.

Then:

4. **Exactly one** eligible `src`; otherwise strip both session keys,
   **fresh** (zero or many ⇒ no guess).
5. If `rs.nodeSessions[src]` matches `{SessionID: id, Backend:
   capabilityBackend}`: `Get(StateRef)` + unpack; set `_session_id`
   and `_session_fingerprint` from **the slot** (authoritative).
6. Else if `HasSession(ctx, id)`: keep `id`; set
   `_session_fingerprint` from `rs.outputs[src]["_session_fingerprint"]`
   (not from an arbitrary inbound value). Missing stamp ⇒ empty
   fingerprint.
7. Else: strip both keys, **fresh**.

Get missing / corrupt blob / header mismatch / unpack error in (5):
strip both keys, no `Task.SessionID`, warn + fresh. Do **not** fall
through to `HasSession`.

`applySessionContinuity` may copy `_session_id` onto the task only
after this helper has run and left the keys in place.

### `runState` plumbing

| Site | Action |
|---|---|
| `newRunState` | empty `nodeSessions` |
| `restoreCheckpointState` | **only** restorer |
| `adoptCheckpointSessions` | **only** `EvictRun` |
| pause resume / `resumeFromFailure` | restore **then** adopt |
| `buildCheckpoint` | copy slots; pause ref from `pauseInfo` |
| fork | omit both refs |
| `execBranch` | no writes; persist illegal in fan-out |

### Resolution order

0. Capability (`EffectiveBackendName` ∈ `{claude_code, pi, codex}`).
   Else strip inbound, fresh.
1. Mid-node pause: claw conversation as today; CLI `Get(pause ref)` +
   unpack. **On Get/unpack failure: strip `_session_id` and
   `_session_fingerprint`, no `Task.SessionID`, warn + fresh.** Do
   **not** overlay `NodeSessions[self]`. Do not leave the id in place
   and hope `--resume` works.
2. Persist own completed-visit slot if Get succeeds; overwrite inbound
   id **and fingerprint from the slot**. Get/unpack failure: strip both
   keys, warn + fresh (same as mid-node).
3. Visit-1 inbound: provenance then slot-or-`HasSession` (above).
4. Fresh.

### C009 / C176 / `--fallback` / fall-through

Unchanged from v5.

### Rewind

Checkpoint without dropped refs **first**, then delete blobs.
`adoptCheckpointSessions` on `resumeFromFailure`. No Epoch. No
runview→executor.

## Key Decisions

1. Persist seam; graph judges/humans stay.
2. Completed visits: `NodeSessions` only (not outputs).
3. In-flight CLI pause: error blob → Put → pause ref.
4. PauseRun error ⇒ **always keep** the new blob. No LoadRun/Delete.
5. Fail-stop; never restore the old slot; `FailRunResumable` carries
   the **new** slot.
6. Inbound: provenance from **source output stamps** (not mapping
   literals); exactly one eligible `src`; then slot or `HasSession`.
   Slot fingerprint wins; HasSession path uses
   `outputs[src]._session_fingerprint`.
7. Get/unpack failure always strips id + fingerprint.
8. `restore` then `adopt`; persist-only NodeSessions; trunk-only;
   C176; whitelist; one PR; CLI not claw; cloud twin.

## Rejected

| Alternative | Why not |
|---|---|
| Delete blob on PauseRun error | Mongo may have committed; a later `LoadRun` is not proof it did not. Keep blob; `DeleteRun` GCs. |
| `HasSession` / "producing edge" without `outputs[src]._session_id == id` | A literal `with { _session_id: "x" }` is edge-produced but not source-emitted. |
| Leave `_session_id` in place after unpack failure | `--resume` of a missing/corrupt pack is a wrong conversation (Pi may mint a new session under the same id). |
| Stamp `_session_state_ref` on output | Loop snapshots / artifacts / fork copies outlive `Delete(old blob)`. |
| Refcount all historical refs | Unnecessary if refs never leave `NodeSessions` / pause cp. |
| Runtime walks `CLAUDE_CONFIG_DIR` | Belongs on the packer (`HasSession`). |
| >1 matching upstream slot → pick any | Wrong conversation. Fresh. |
| Restore old slot on checkpoint fail | Same-process loop resumes a left conversation. |

## Runtime sketch

1. DSL as before.
2. Store + cloud twin.
3. Error blob → Put → PauseRun; **never delete on PauseRun error**.
4. Success-path `takeSessionStateBlob`; persist slot commit
   fail-stop with new slot in `FailRunResumable`.
5. Visit-1: stamp-eligible `src` then slot or `HasSession`.
6. restore then adopt on both Resume paths.

### Tests (mandatory)

| Test | Asserts |
|---|---|
| Parse/unparse/jsonenc; iota `SessionFork` unchanged | |
| C009 / C176 / ApplyRunFallback / fan-out persist error | |
| Visit 1 fresh, visit 2 same id, no `_session_id` in `with` | |
| Visit 2 conflicting inbound; own slot wins | |
| Wiped dir between completed visits | |
| ask_user error carries blob; checkpoint has ref not bytes | |
| Wiped dir between ask_user and answer; pause ref not NodeSessions | |
| Two `ask_user`s: success deletes A after B committed | |
| PauseRun writes checkpoint then returns error: **B not deleted** | **Rev7 #1** |
| PauseRun(B) error: **A and B both kept** (no LoadRun delete) | |
| Non-persist reInvoke success: no NodeSessions | |
| Persist success then SaveCheckpoint fail: no edge; `FailRunResumable` cp has **new** slot; Resume re-executes from that slot | **Rev6 #4** |
| Inbound id, wiped dir, **no** matching `nodeSessions[src]`: no Task.SessionID | |
| Inbound id, one stamp-eligible source **slot**, wiped dir: unpack + SessionID + **slot fingerprint** | **Rev7 #4** |
| Source output `sess-real`, edge injects literal `sess-other`, `HasSession(sess-other)=true` ⇒ fresh | **Rev8** |
| Source output id valid, `HasSession=true`, **no** slot, inbound fingerprint contradicts `outputs[src]._session_fingerprint` ⇒ fingerprint from **source output** | **Rev8** |
| Inbound id matches **two** stamp-eligible sources: fresh | |
| Literal `_session_id` in `with {}` + `HasSession` true + source output id **differs** (or missing): fresh | **Rev8** |
| Pause-ref Get/unpack fail: stripped id/fingerprint, no Task.SessionID | **Rev7 #3** |
| Completed-slot Get/unpack fail: stripped id/fingerprint, no Task.SessionID | **Rev7 #3** |
| Output maps / loop snapshots / fork outputs contain **no** `_session_state_ref` | **Rev6 #2** |
| `HasSession` false + no slot ⇒ no SessionID even if inbound id set | **Rev6 #3** |
| Auth files not packed; `../` refused | |
| Rewind + same executor + stale RAM + `resumeFromFailure` | |
| Store conformance fs + mongo/S3 | |
| kimi/grok persist: capability false | |

No test may pass only because `~/.claude` still held the jsonl.

## PR Plan

One PR: `dsl,runtime,store: session persist resumes a node's own CLI session (StateRef)`.

Not in this PR: film/shorts; catalog bots; kimi/grok resume; claw
`conversation_ref`; studio loop collapsing.

Parse-only or twin-less land forbidden.

## Follow-ups / out of scope

Unchanged.

## Consequences

- Persist writer survives graph human and in-node `ask_user` on a new
  pod.
- A PauseRun/network error never deletes a blob the durable
  checkpoint might still name.
- Visit-1 inherit across recycle uses the **source node's current
  slot**, not a stamped ref that could dangle.
- Campaign/plan `fresh` unchanged.

## Open questions

Implementer discovery: Claude jsonl layout with two config dirs;
Codex per-id file set; numeric caps from tool-blob ceiling.

## Reviewer checklist (revision 8)

- [ ] Visit-1 provenance uses `outputs[src]["_session_id"]` (and
      `_backend`), not merely the edge mapping result. Literal
      `sess-other` with `HasSession(sess-other)` and source
      `sess-real` ⇒ fresh.
- [ ] HasSession path fingerprint from `outputs[src]`; slot
      fingerprint still authoritative when a slot exists.
- [ ] v7 items still hold: PauseRun never Deletes; Get/unpack strips
      keys; fail-stop durable new slot; no ref on outputs; whitelist;
      cloud twin; C176; fan-out; one PR; CLI not claw.
