# Flow — the assistant dock, run watches and the session board

Three assistant surfaces meet at one seam: a bot's chat manifest declares a human pause with two doors, one for the operator's typed text and one for a host-attested event, and a durable run watch is what walks a watched run tree and knocks on the second door.

> Drawn from the code at commit `f564846d3`; how this page is kept true: [README](README.md).

## The seam: one pause, two doors

A bundle's `chat:` block says, per workflow node, how that node appears in a transcript. A `human` node is the operator's turn. `validateChatSurface` **fails the bundle** rather than warning when a node names an unknown kind, when a non-human node declares an answer field, or when a human node names neither `text_field` nor `approved_field` — the failure it prevents is a chat window that looks alive and swallows every message. `host_event_field` is the second door on that same pause: a field reserved for host-attested input, deliberately apart from `text_field` so automatic input can never read as operator speech or as an authorization.

Three surfaces stand over that seam. The **dock** is studio-side and holds no server state: `ChatDock` and `AssistantProvider` mount outside the route switch, the chat registry is built from each bundle's `chat` block served by `GET /api/v1/bots`, and a model's requested actions are re-validated host-side against a closed catalogue under a per-action policy in `localStorage`. The **run watch** is server-owned and durable: one coordinator goroutine mints episodes and force-resumes the assistant at its verified chat boundary. The **session board** is per-run, and only its second layer costs a token.

## A run watch, from arm to the woken turn

Every bug this path has is a handoff bug: who writes the row, who nudges the coordinator, who owns the cooldown and the lease — and above all that the *host*, not the operator, resumes the assistant through the second door of the same pause, with `operator_authorized: false`.

```mermaid
sequenceDiagram
  participant OP as operator<br/>in the studio
  participant DOCK as studio dock<br/>ChatDock + AssistantProvider
  participant API as server handler<br/>handleCreateAssistantWatch
  participant CO as watch coordinator<br/>one goroutine, 20s ticker
  participant WS as runwatch store<br/>watches + episodes
  participant TGT as the watched run tree
  participant COPI as the assistant run<br/>parked on its chat node

  OP->>DOCK: asks the assistant to keep an eye on a run
  DOCK->>DOCK: validate the model's run.watch request against the closed catalogue,<br/>rebuild the payload from allowed fields - a model-supplied force is stripped
  DOCK->>API: POST api/runs/.../assistant-watches - assistant_run_id, mode, kinds, cooldown
  Note over API: refuses: an unsafe origin, a cross-store write, mode auto_safe,<br/>more than four kinds, a synthetic caller, another operator's assistant,<br/>a tenant mismatch, a bot with no host-event chat node
  API->>WS: findCoveringAncestorWatch - does this assistant already watch an ancestor
  WS-->>API: it does - widen that policy MONOTONICALLY in place, answer covered_run_id
  API->>WS: otherwise CreateWatch - active, the cursor set to the target's LastSeq,<br/>TreeTrackingStartedAt now, under the unique index one_active_owner_target
  API->>COPI: PublishVeilleArmed, on the ASSISTANT's own event log
  API->>CO: nudge - at most one extra sweep is ever queued
  DOCK->>API: the standby event is a doorbell - GET api/runs/.../watching
  API-->>DOCK: BOTH channels - run_watches and watched_issue_ids

  loop every 20s, plus one sweep per run event and one at startup
    CO->>WS: ListActivePage in created_at order, to a fixed upper bound
    CO->>TGT: load the root, then walk ListChildRuns breadth-first into a descendant tree,<br/>capped at 5000 nodes
    CO->>WS: EnsureRunObservation - one durable cursor per descendant
    CO->>TGT: observe paused human gates, stall health events, terminal states
    CO->>WS: CreateEpisode keyed on the watch and the outcome event id -<br/>the unique index watch_outcome IS the de-duplication
  end

  CO->>WS: ListDueEpisodes, then ClaimEpisode under a lease and a compare-and-swap,<br/>so two replicas cannot buy two assistant turns for one outcome
  CO->>COPI: safeAssistantWatchPause - the run paused on a human gate, its checkpoint<br/>IS the manifest chat node, BackendPendingToolUseID empty
  Note over CO,COPI: defers instead of failing - ten release reasons, drawn in the episode<br/>lifecycle below, plus the resume error itself. Eight back off exponentially, cooldown<br/>resumes at last_delivered_at plus the cooldown, and chat_history_projection_failed<br/>retries at a flat 20s
  CO->>COPI: Resume, Answers host_event_field = the envelope: target_run, outcome_run,<br/>authority iterion-host, operator_authorized false, and Force true
  COPI-->>CO: the assistant takes a turn nobody typed
  CO->>WS: CompleteEpisode - and only the ROOT reaching run.finished resolves the watch
```

## Steps

| Actor | Action | Code |
|---|---|---|
| user | opens the dock from any authenticated route; it survives navigation because it mounts outside the route switch, and its presentation is persisted per user, not per route | `ChatDock`, `AssistantProvider`, `openedDock` |
| studio | resolves which bot answers from the manifest-driven registry, refusing the bot that owns the full-width route; one predicate decides whether the dock renders at all there | `useChatRegistry`, `resolveDockBot`, `dockStandsDown` |
| studio | mints the page-context chip and every dropped reference through ONE vocabulary: an id whose shape it rejects is refused, not repaired | `mintReference`, `referenceDragProps`, `addReferenceToDrag` |
| server | turns typed pointers into a bounded host-attested snapshot; the public error is generic so a store path cannot leak through it | `handleAssistantContextResolve`, `resolveAssistantReference` |
| studio | validates the model's action request against the closed catalogue and resolves the saved policy — deny, ask, explicit, allow | `validateAssistantActionRequest`, `decideAssistantAction` |
| server | gates the arm, then either widens a covering ancestor watch or creates the row | `handleCreateAssistantWatch`, `findCoveringAncestorWatch`, `mergeAssistantWatchPolicy` |
| store | holds one active watch per tenant, owner and target; the duplicate drives the idempotency, reconfigure and handoff ladder | `CreateWatch`, `ReconfigureActiveWatch`, `TransferActiveWatch` |
| server | writes the standby events on the ASSISTANT's log, so its transcript explains why it went quiet and later why it spoke | `publishVeille`, `store.PublishVeilleArmed` |
| server | nudges the coordinator from the bus — the callback does no store I/O, so unsubscribe cannot block — and sweeps anyway on a ticker, because the bus is lossy by design | `handleEvent`, `nudge`, `sweepLoop` |
| server | arms the second channel too: a terminal run whose card an assistant watches gets a watch on ARRIVAL, with narrower kinds than an operator may request | `armWatchesForTargetFrom`, `autoWatchKinds`, `reconcileArmedWatches` |
| server | walks the tree, keeps a cursor per descendant, and mints one episode per actionable outcome | `loadWatchTree`, `ensureTreeObservations`, `observePausedHumanGate`, `observeRunHealthInTree`, `observeTerminalForWatch` |
| server | claims one episode, then re-checks the delivery gate — capability is not permission to deliver | `deliverDue`, `attempt`, `safeAssistantWatchPause` |
| engine | resumes the assistant at its chat node with the host envelope, and the studio's own host events take the same admission | `runview.Service.Resume`, `deliverHostEventWithService` |
| studio | treats the standby events as a doorbell, re-reads both channels, and offers ONE stop that cuts both | `useRunVeille` |
| server | suppresses "your run is waiting on you" while an assistant is standing by | `hasActiveVeille` |
| cli | promotes one exact active watch into a bounded receipted mission over `run.resume` and `run.rewind` — the only client of those four routes | `iterion remote runs mission`, `handleCreateAssistantMission` |
| engine | spawns the session-board curator next to the declared supervisors, for the run's lifetime | `startSessionBoard`, `startDeclaredSupervisors` |
| studio | renders the deterministic task list always, the curated widgets only when the server says curation is on | `SessionBoardTab`, `useSessionBoardWidgets` |

## The episode lifecycle

An episode is one actionable outcome of one watch. Its id is derived from the watch and the outcome event id, so the fast path and the reconciliation sweep cannot double-deliver; a release is a deferral, not a failure.

```mermaid
stateDiagram-v2
%% vocabulary: runwatch.EpisodeState
    [*] --> pending: CreateEpisode, next_attempt_at now - a duplicate id is simply not created
    pending --> processing: ClaimEpisode sets lease_owner and lease_until, 45s
    processing --> pending: ReleaseEpisode, exponential backoff - assistant_unavailable,<br/>assistant_busy, assistant_paused_by_operator, assistant_failed_resumable,<br/>assistant_not_at_chat_boundary, assistant_budget_near_cap,<br/>target_unavailable, outcome_run_unavailable
    processing --> pending: ReleaseEpisode on a fixed delay - cooldown at last_delivered_at<br/>plus the cooldown, chat_history_projection_failed at a flat 20s, and a failed resume<br/>under its own error message truncated to 300 runes
    processing --> done: CompleteEpisode after the assistant resume returns
    processing --> done: completed as STALE - a cancelled outcome on a target no longer<br/>cancelled, or a paused gate the operator already answered
    processing --> blocked: BlockEpisode - the watch is no longer active, a tenant mismatch,<br/>or the assistant reached a terminal status
    processing --> processing: a lost CompleteEpisode writes no state - the row stays claimed,<br/>and ListDueEpisodes then ClaimEpisode re-claim it in place once lease_until lapses
    done --> [*]
    blocked --> [*]
```

| State | Means | Written by |
|---|---|---|
| `pending` | due, or waiting out the `next_attempt_at` a release set | `CreateEpisode`, `ReleaseEpisode` |
| `processing` | one worker holds the lease and is testing the delivery gate | `ClaimEpisode` |
| `done` | delivered — or deliberately dropped as stale, which keeps the watch itself alive | `CompleteEpisode` |
| `blocked` | the capability it was minted against is gone; never retried | `BlockEpisode` |

## The watch lifecycle

`createWatch`, `transferWatch` and `stopWatch` are the only writers, so the event the dock renders cannot drift from the state it describes.

```mermaid
stateDiagram-v2
%% vocabulary: runwatch.WatchState
    [*] --> active: CreateWatch, or an auto-arm from a watched card
    active --> active: ReconfigureActiveWatch widens the policy - a union of kinds,<br/>propose dominating diagnose, the shorter cooldown winning
    active --> active: TransferActiveWatch swaps only the assistant, keeping the<br/>ledger and the cursors - the outgoing dock gets watch_transferred
    active --> resolved: the watched ROOT reached run.finished - a child finishing is<br/>internal progress and stays silent
    active --> stopped: the operator pressed stop, the target is missing,<br/>or the assistant finished, failed or was cancelled
    resolved --> [*]
    stopped --> [*]
```

## The session board — two layers

```mermaid
flowchart TD
    EV["a run emits tool_started whose tool is exactly TodoWrite or todo_write"] --> FOLD["the studio folds it into todoHistoryByExec, which is NEVER cleared on node_finished, so the journey survives the run"]
    FOLD --> TAB["layer 1: the Tasks tab renders the task-list timeline unconditionally, for every run and every backend that writes those events"]
    LAUNCH["the run goroutine reaches startSessionBoard, beside startDeclaredSupervisors"] --> G1{"a spec store exists - it needs an on-disk storeDir, so never in cloud mode"}
    G1 -- "nil" --> OFF["layer 2 off: the route still answers a zero-value spec, version 0 and no widgets, never a 404"]
    G1 -- "built" --> G2{"ITERION_SESSION_BOARD is on, true, 1 or yes"}
    G2 -- "unset, the default" --> OFF
    G2 -- "on" --> G3{"the run's frozen pool stamp is readable"}
    G3 -- "unreadable" --> REF["refusing to start, a Warn only - the same refusal gates declared supervisors"]
    G3 -- "read" --> G4{"on a pool-stamped run the curation model must be gateway-routed"}
    G4 -- "a vendor model" --> REF
    G4 -- "admitted" --> CO["sessionboard.Coordinator: one goroutine over the run's events, turn boundaries debounced 3s, a 45s cooldown floor, MaxEvals 20"]
    CO --> DEC{"the evaluator answers a BoardDecision of widget DIFFS"}
    DEC -- "an unknown kind or an empty id" --> DROP["that upsert is skipped - a hallucinated kind cannot poison the spec"]
    DEC -- "nothing actually changed" --> NOOP["ApplyDecision answers changed false: no version bump, no write, no front-end churn"]
    DEC -- "the widget set changed" --> PUB["version + 1, updated_seq stamped from the coordinator's lastSeq, saved atomically beside the run's other files"]
    PUB -- "the save failed" --> KEEP["the coordinator keeps mirroring the last PERSISTED spec, so the next eval re-derives the diff instead of believing a lost update landed"]
    PUB --> TAB2["the tab fetches the spec only when the server reports curation enabled, refetching on a 1.5s debounce as lastSeq advances"]
    TERM["a terminal event"] --> LAST["one last eval that bypasses the cooldown but not MaxEvals, then the coordinator stops"]
```

## Guards that refuse

- **The bundle's own gate** — `validateChatSurface` fails the bundle on an unknown node kind, an answer field on a non-human node, a human node with neither `text_field` nor `approved_field`, or no human node at all; a rejected block costs that bundle its catalog entry while its valid siblings still list.
- **The arm's origin and store** — `requireSafeOrigin` plus `rejectCrossStoreWrite` on every watch and mission create and stop: `decodeJSONCapped` does not check `Content-Type`, so without them any origin could arm an LLM-spending watch with a preflight-free `POST` at the loopback studio.
- **The policy** — `400` for a mode other than `diagnose` or `propose`, naming `auto_safe` as unavailable until server-side action authority exists; `400 invalid watch event kinds` beyond four, on a duplicate, or outside the five; `400 invalid watch limits` for a `max_episodes` outside `0–20` or a `cooldown_seconds` outside `0–86400`; a `max_episodes` inside is normalized to `0`, because a watch is now lifetime-bound.
- **The operator and the pair** — `400` when the target and assistant run ids are missing or equal, `404` for a target or assistant that does not resolve, `403` for a synthetic identity, for an assistant owned by another operator and for a tenant mismatch, `409` when the assistant is already finished, failed or cancelled.
- **Capability** — `409 assistant is not host-event capable`: `resolveAssistantChatCapability` reads the manifest and deliberately not the checkpoint, because arming usually happens mid-turn and reading the checkpoint there would silently skip a perfectly capable assistant.
- **Coverage and the incumbent** — `409 covering ancestor watch changed; retry the request` rather than manufacturing a nested watch from a stale decision; `409 this run already has an active assistant watch` when the incumbent is queued or running, or cannot be loaded at all.
- **The native retry** — no episode is minted for a `failed_resumable` target whose `RetryState.RetryAfter` is set: waking a model there would diagnose a failure already scheduled to disappear.
- **The young or covered stall** — a stall newer than one sweep interval, a stall with a matching `stall_recovered` twin, and a stall whose tree still holds a progressing descendant are all suppressed. The young stall and the one with a progressing descendant leave the cursor *before* the event, so the next pass re-evaluates it; a stall whose `stall_recovered` twin is already in the loaded page is settled, so the cursor advances past it.
- **The delivery gate** — `safeAssistantWatchPause` requires the assistant parked on a human gate, its checkpoint equal to the manifest's host-event chat node, and `BackendPendingToolUseID` empty. A checkpoint on an agent node is a mid-turn `ask_user`: the episode is released, never delivered, so the pending question is not stolen. The same admission gates `POST /api/runs/{id}/host-event`, which stamps `kind`, `authority` and `operator_authorized` **last** so a caller cannot claim operator authorization through the body.
- **The run-surface ladder** — every `/api/runs/{id}/…` route here crosses `scopeRunByID` before its handler: `GET`/`HEAD` take a viewer, every `POST`/`DELETE` takes a member; no standing answers `404` with no existence oracle, a wrong rung `403`, a membership-store failure `500`. `DELETE /api/assistant-watches/{watchID}` carries no wildcard under `api/runs` and is therefore outside that class: it rests on `requireAuth` plus the handler's own watch and run load.
- **The mission** — `409` under detached local mode, since a subprocess wire cannot preserve receipt identity; `400` for an action outside `run.resume` and `run.rewind` or listed twice, a TTL outside 60–86400 or `max_actions` outside 1–20; `409 mission requires exactly one active exact watch`, `409` when the target run already holds an active mission, and `409` for a `watch_id` that is not an active exact watch of this operator or an invocation key already bound to a different mission policy; and a receipt rejected with *rewinding the workflow entry is outside mission authority*.
- **The curation layer** — an unreadable pool stamp or a curation model that is not gateway-routed on a pool-stamped run both refuse the coordinator outright, logged `Warn` only; unknown widget kinds are dropped on apply and ignored on render.

## Collections touched

`assistant_run_watches` — the durable target→assistant link; `one_active_owner_target` is unique and partial on `state: active`, alongside `target_active`, `assistant_active` and `active_creation_order`. `assistant_run_watch_episodes` — one row per actionable outcome; `watch_outcome` unique on the watch and the outcome event id, `due_claim` on state, next attempt and lease. `assistant_missions` — `mission_invocation` unique per tenant, operator and invocation key, `one_active_target` unique and partial. `runs` — read for status, checkpoint, owner, tenant and `WatchedIssueIDs`, which is the *card* standby channel; written by the resume that delivers an episode. `events` — the standby events written on the ASSISTANT's log, and the run-health events the per-run cursor reads. `interactions` — the pending question the engine writes when a run parks on a human node; the delivery re-check never reads it, `pausedGateStillActive` loads the paused RUN and compares its checkpoint against the episode's `paused_node_id` and `paused_interaction_id`. `user_messages` belongs to the opposite surface: steering, and every supervisor intervention, queues a message there instead of resuming anything.

The session-board spec is **not** a collection: `sessionboard.FileStore` writes `runs/<id>/sessionboard.json` under the run store's base directory, and cloud mode, having no `storeDir`, has no twin — the route answers a zero-value spec there.

## Related

- [assistant-dock.md](../assistant-dock.md) — the dock's home page: the presentation states, the chat registry, the action policy table, the context chip, run watches and episodes, missions and the standby chip.
- [session-board.md](../session-board.md) and [supervisors.md](../supervisors.md) — the two layers of the Tasks tab, and the sibling coordinator whose decision queues a steering message instead of a widget diff.
- [adr/091-ubiquitous-assistant-chat-dock.md](../adr/091-ubiquitous-assistant-chat-dock.md) — why the dock and its session mount above the route tree, and why assistant and steering are named apart on a run page.
- [data-model-runs.md](data-model-runs.md) — `runs`, `events`, `interactions`, `user_messages`, the three `assistant_*` collections and the run status machine; [access-control.md](access-control.md) — the run-surface ladder and the principal kinds behind `requireAuth`.

