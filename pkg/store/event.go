// Package store implements the file-backed persistence layer for iterion runs.
// It manages the lifecycle of runs, their events, artifacts, and human
// interactions using a local filesystem layout:
//
//	runs/<run_id>/run.json
//	runs/<run_id>/events.jsonl
//	runs/<run_id>/artifacts/<node>/<version>.json
//	runs/<run_id>/interactions/<interaction_id>.json
package store

import "time"

// ---------------------------------------------------------------------------
// Event — timestamped fact emitted by the runtime
// ---------------------------------------------------------------------------

// EventType enumerates the minimum events to persist per the V4 plan.
type EventType string

const (
	EventRunStarted     EventType = "run_started"
	EventBranchStarted  EventType = "branch_started"
	EventBranchFinished EventType = "branch_finished"
	// EventBranchAbandoned marks a fan-out branch still running when the
	// collector stopped waiting for it (cancelled fan-out + grace period
	// elapsed — a branch wedged in executor.Execute ignoring ctx). The
	// branch has no branch_finished; consumers use this to close the
	// in-flight gauge and surface the potential resource leak.
	// Data: {router, mode, grace_period, reason}
	EventBranchAbandoned    EventType = "branch_abandoned"
	EventNodeStarted        EventType = "node_started"
	EventLLMRequest         EventType = "llm_request"
	EventLLMPrompt          EventType = "llm_prompt"
	EventLLMRetry           EventType = "llm_retry"
	EventNodeRecovery       EventType = "node_recovery"
	EventNodeVerifiedAction EventType = "node_verified_action" // data: {rung, postcondition_met, policy}
	EventLLMStepFinished    EventType = "llm_step_finished"
	// EventAssistantText carries an agent's mid-turn narration (the
	// assistant's prose between tool calls) so conversation views can
	// render the agent "talking" while it works, on both backends
	// (claude_code streams no llm_step_finished). Structured-payload
	// text (the node's JSON answer) is filtered at the emitter.
	// Data: {text, iteration}
	EventAssistantText   EventType = "assistant_text"
	EventLLMCompacted    EventType = "llm_compacted"
	EventToolStarted     EventType = "tool_started"
	EventToolCalled      EventType = "tool_called"
	EventToolError       EventType = "tool_error"
	EventArtifactWritten EventType = "artifact_written"
	// EventPlanWritten marks a new persisted plan snapshot (an agent's
	// TodoWrite/todo_write living TODO list, captured to runs/<id>/plans/).
	// Best-effort + additive; the studio Plans panel refreshes on it.
	// Data: {seq, node_id, iteration, count}
	EventPlanWritten          EventType = "plan_written"
	EventHumanInputRequested  EventType = "human_input_requested"
	EventRunPaused            EventType = "run_paused"
	EventHumanAnswersRecorded EventType = "human_answers_recorded"
	// EventInteractionAnswered fires when a pending ASYNC interaction
	// (ADR-081, Interaction.Kind == "async") receives its answer — while
	// the run keeps executing. Distinct from human_answers_recorded, which
	// marks the answers of a blocking pause at resume time. Data:
	//   - interaction_id, node_id (the asking node), async: true
	EventInteractionAnswered EventType = "interaction_answered"

	// asyncEventDataKey marks a human_input_requested event as a
	// NON-BLOCKING async question (ADR-081) — see IsAsyncHumanInput.
	asyncEventDataKey           = "async"
	EventRunResumed   EventType = "run_resumed"
	// EventRunSteered marks a live-steering intervention on a RUNNING
	// run (bump_loop / raise_budget), emitted by the engine goroutine
	// atomically with the in-memory mutation so the timeline and any
	// reconnecting WS subscriber see exactly what was applied. Data:
	//   - command: "bump_loop" | "raise_budget"
	//   - target: loop name (bump_loop) or "" (raise_budget)
	//   - delta: extra iterations granted (bump_loop)
	//   - applied / effective: post-apply values (per-command shape)
	//   - operator: issuing principal when known ("" for local CLI)
	EventRunSteered EventType = "run_steered"
	// EventRunHealth is the PERSISTED twin of the ephemeral alert-broker
	// event: stall / stall_recovered / budget / failure alerts appended
	// to events.jsonl so the timeline and a reconnecting WS subscriber
	// keep the episode. The alert Manager ignores this type on Observe
	// by construction (no detection feedback loop). Data:
	//   - kind: alert kind ("stall" | "stall_recovered" | …)
	//   - reason / axis / budget_pct: mirror the alert fields
	EventRunHealth EventType = "run_health"
	// EventRunAutoResumed marks a bounded run-level auto-resume (the
	// `--auto-resume N` / ITERION_AUTO_RESUME loop). Distinct from
	// EventRunResumed (operator-initiated) so the timeline shows the
	// automation. Data:
	//   - attempt: 1-based auto-resume attempt
	//   - max: the configured N
	//   - code: the RuntimeError code that triggered it (EXECUTION_FAILED,
	//     BUDGET_EXCEEDED, TIMEOUT, RATE_LIMITED, NETWORK_TRANSIENT, …)
	//   - delay_ms: backoff waited before this attempt
	//   - reason: "auto"
	EventRunAutoResumed EventType = "run_auto_resumed"
	// EventRunRetryScheduled marks a durable retry armed for a run that
	// failed on an exhausted provider quota window. It is the "we are
	// waiting, not dead" marker: without it a failed_resumable row that
	// will resume in 33h is indistinguishable from one that never will.
	// The resume itself emits EventRunAutoResumed, so the pair reads the
	// same on the timeline as the CLI's in-process auto-resume loop. Data:
	//   - code: the RuntimeError code that armed it (USAGE_LIMIT_BLOCKED)
	//   - reason: the failure class ("usage_window")
	//   - retry_after: RFC3339 instant the retry becomes eligible
	//   - attempt / max_attempts: position in the run's attempt budget
	//   - reset_source: how the instant was derived ("typed_error",
	//     "runtime_code+parsed_text", "…+blind_wait") — the degraded
	//     paths must be visible, not silent
	EventRunRetryScheduled EventType = "run_retry_scheduled"
	// EventUsageCap marks the provider's subscription telemetry crossing a
	// cap the OPERATOR set, below the provider's own wall (see
	// pkg/usagecap). It is the difference between "the provider refused
	// us" and "we stopped ourselves", which nothing else on the timeline
	// can tell apart — both end the run the same way. Data:
	//   - window: the provider's window name (five_hour, seven_day, …)
	//   - family: the cap that governs it ("5h" | "week")
	//   - percent / cap: observed utilization vs the configured ceiling
	//   - mode: "soft" (nothing new starts) or "hard" (this run stops)
	//   - stopped: whether THIS run was ended by the cap
	//   - resets_at: RFC3339 instant the window reopens, when known
	EventUsageCap EventType = "usage_cap"
	// EventRunWorkspaceReset marks a repo-backed run RE-EXECUTING from a
	// FRESH clone. The runner deletes the per-run repo dir when a run
	// returns and re-clones on every claim, so a second attempt never
	// inherits the first one's working tree: whatever an earlier node edited
	// but did not commit is gone. The checkpoint restores node OUTPUTS, so a
	// downstream node still reads "the alignment was applied" while the
	// files that carried it no longer exist — a divergence that is otherwise
	// completely silent.
	//
	// Keyed on the checkpoint existing, not on the delivery carrying a resume
	// spec: a redelivery of a run still marked `running` (a pod that died
	// inside the orphan sweeper's window) re-clones the same way and would
	// otherwise discard the work unannounced. Emitted once per claim, so N
	// markers mean N delivery attempts — matching EventRunResumed, which the
	// engine also emits per attempt. Data:
	//   - reason: which fact made this a re-execution — "resume" (an explicit
	//     resume publish) or "redelivery" (a checkpoint with no resume spec)
	//   - repo_url / repo_sha: what the clone was re-anchored on
	EventRunWorkspaceReset EventType = "run_workspace_reset"
	// EventRunBankRefused marks THIS attempt's head being dropped by the
	// runner's death bank while an EARLIER attempt of the same run keeps
	// the storage branch — because that attempt banked a strictly richer
	// chain, because this attempt's workspace failed the integrity
	// check, or because this attempt's push failed.
	//
	// It exists because the refusal is otherwise invisible outside the pod
	// log: FinalBranch/FinalCommit keep naming a valid, forge-backed,
	// mergeable pair — just a DIFFERENT attempt's — so the operator sees a
	// head their run's own artifacts and gate never cite, and `runs merge`
	// merges that other attempt's tree. FinalBranchError cannot carry it:
	// that field's documented meaning is "FinalCommit has no persistent
	// branch guarding it", which is exactly not the case here. Data:
	//   - branch: the storage branch that was left alone
	//   - kept_head: the head that branch stays on
	//   - kept_commits / dropped_head / dropped_commits: the two chains'
	//     exclusive counts and this attempt's head — the chain-comparison
	//     refusal only
	//   - cause: why this attempt's workspace was refused — the
	//     integrity-check refusal only
	//   - reason ("push_failed") / error: the forge refused this
	//     attempt's push — the push-failure refusal only
	EventRunBankRefused EventType = "run_bank_refused"
	// EventRunBankSuperseded marks a finished outcome force-taking the
	// storage branch from an earlier dead attempt whose banked chain the
	// finished chain does NOT contain. The takeover itself is correct —
	// the storage branch must point at the finished product — but the
	// dropped chain may be the only forge-side copy of that attempt's
	// work, so the bank archives it first and this event says where (or
	// why it could not). Emitted only on divergence: a banked head the
	// finished chain contains is not a loss and stays silent. Data:
	//   - branch: the storage branch the finished head took over
	//   - superseded_head / new_head: the dropped and the winning heads
	//   - archived_ref: the iterion/run-<id>-attempt-<sha12> ref now
	//     holding the dropped chain — the success shape
	//   - archive_error: why the chain could not be archived (it stays
	//     recoverable from the run's git-meta snapshot) — the failure
	//     shape; exactly one of the two is present
	EventRunBankSuperseded EventType = "run_bank_superseded"
	// EventRunBankAttempt marks an attempt's work being parked on its own
	// uniquely-named ref (iterion/run-<id>-parked-<sha12> — a distinct
	// infix from the supersede archives' -attempt-, so a pruning policy
	// can tell a dead attempt's archive from a live run's parked work by
	// name alone) because the
	// STORAGE branch must not be touched: an interrupted delivery (the
	// lease may already belong to another pod — a ref named after this
	// chain's own head cannot contest anything), a paused run (recording
	// FinalBranch would make a half-done run merge-eligible), or a
	// bankable death whose run ctx was cancelled for lease loss. The run
	// doc is deliberately left alone — no FinalBranch/FinalCommit/
	// FinalBranchError — so this event is the ONLY durable record that
	// the ref exists (or why it could not be pushed). Data:
	//   - ref / head: the parked ref and the commit it holds — the
	//     success shape
	//   - cause: which outcome parked it (interrupted, paused,
	//     paused_operator, or the lease-loss death)
	//   - error: why the head could not be resolved or pushed (the work
	//     stays recoverable from the git-meta snapshot) — the failure
	//     shape; exactly one of ref/error is present
	EventRunBankAttempt EventType = "run_bank_attempt"
	// EventRunRewound marks an in-place rewind: the operator re-anchored
	// THIS run's checkpoint on an already-executed node and invalidated
	// the outputs downstream of it, so the next resume re-executes from
	// there (see runview.Service.Rewind). Distinct from a fork, which
	// mints a NEW run id and leaves this one untouched.
	//
	// The event is the audit trail: events.jsonl stays append-only, so
	// the superseded node_started / node_finished records of the dropped
	// nodes remain in the timeline with this marker explaining why they
	// are about to repeat. Data:
	//   - from_node: the checkpoint node the run was anchored on before
	//   - to_node: the new anchor (== NodeID on the event)
	//   - dropped_nodes: node ids whose outputs were invalidated,
	//     sorted; always includes to_node
	//   - tombstoned_artifacts: dropped nodes whose published artifact was
	//     superseded by a `rewound` marker version, sorted
	//   - orphaned_child_runs: subbot child run ids whose pointer was
	//     released, sorted
	//   - promoted_from: the requested pivot when it was promoted to the
	//     router of the fan-out containing it ("" otherwise)
	//   - files_reverted: whether the workspace was reverted to the pivot's
	//     pre-execution snapshot
	//   - files_ref / files_revert_commit / files_backup_ref: the snapshot
	//     ref restored, the revert commit written on top of HEAD, and the
	//     ref banking the pre-revert state
	//   - files_skip_reason: why the workspace was left untouched (no
	//     worktree, restore scope `none`, no recorded pre-boundary, an
	//     unwalkable snapshot chain, or an empty scope — nothing this run
	//     is recorded to have changed after the pivot started)
	//   - files_restore_scope: the breadth applied ("produced" | "full" |
	//     "none")
	//   - files_scope_count: how many workspace paths the scope admitted
	//   - files_overwritten / files_overwritten_paths: the count, and the
	//     names (capped at workspacetrack.ReportPathCap), of in-scope
	//     paths the restore took whose disk content matched neither side
	//     of the run's recorded range — work that arrived after the run
	//     stopped recording it
	//   - files_left_in_place / files_left_in_place_paths: the same pair
	//     for paths that changed since the run's last recorded boundary
	//     and were NOT restored
	//
	// The last four exist because a rewind driven over the API or by an
	// agent never sees the CLI's stderr: without them the audit trail
	// cannot answer "what did that rewind take from me".
	EventRunRewound EventType = "run_rewound"
	// Review-&-merge gate events (interaction: review). The gate runs a
	// companion↔human dialogue and squash-merges during the pause.
	EventReviewTurn     EventType = "review_turn"    // data: {interaction_id, role, turn}
	EventReviewVerdict  EventType = "review_verdict" // data: {decision, confidence, blockers}
	EventReviewMerged   EventType = "review_merged"  // data: {final_commit, merged_into, strategy}
	EventJoinReady      EventType = "join_ready"
	EventNodeFinished   EventType = "node_finished"
	EventEdgeSelected   EventType = "edge_selected"
	EventBudgetWarning  EventType = "budget_warning"
	EventBudgetExceeded EventType = "budget_exceeded"
	// EventBudgetExitGrace records that a node ran on a SPENT budget
	// because it sits on the run's exit path — data: {dimension, used,
	// limit, node}. A run that emits it has, deliberately, spent past
	// what it declared: the operator must be able to see that in the
	// events, not only in the invoice.
	EventBudgetExitGrace EventType = "budget_exit_grace"
	EventRunFinished     EventType = "run_finished"
	EventRunFailed       EventType = "run_failed"
	EventRunCancelled    EventType = "run_cancelled"
	// EventAlert is an in-process-only run-health alert (stall, budget,
	// failure) fanned out to studio browser sessions via the event
	// broker. It is NEVER persisted to events.jsonl — the alert Manager
	// publishes it straight to the broker to avoid a detection feedback
	// loop — so it carries no seq and is observational only.
	EventAlert EventType = "alert"
	// EventRunInterrupted is emitted when the studio server drains in-flight
	// runs during shutdown (SIGTERM, watchexec rebuild, etc). The companion
	// run.json status flips to failed_resumable so the next boot can offer
	// one-click resume — distinct from EventRunCancelled (user-initiated).
	EventRunInterrupted EventType = "run_interrupted"
	// EventDelegateStarted / Finished / Error / Retry are the CLI-backend
	// lifecycle. Unlike llm_request (claw-only), these fire for every
	// delegate backend. Data always carries `backend`. Started also
	// carries `declared_model` when the node asked for one. Finished /
	// Error additionally carry `effective_model` (what the provider
	// reported), `context_window`, `max_output_tokens`, `context_used`
	// — omitted when unknown, so absence ≠ zero.
	EventDelegateStarted  EventType = "delegate_started"
	EventDelegateFinished EventType = "delegate_finished"
	EventDelegateError    EventType = "delegate_error"
	EventDelegateRetry    EventType = "delegate_retry"

	// EventModelFallback is emitted once each time a node's fallback
	// chain falls through from a failed element to the next one — a
	// route change, not a failure: the run continues. Distinct from
	// EventDelegateRetry, which re-issues against the SAME element.
	//
	// Data keys: from_backend, to_backend, from_model, to_model,
	// from_provider, to_provider (the credential hints, "" = auto),
	// reason (delegate.FallbackCategory), attempts (budget spent on the
	// failed element), error. A launch-time run fallback also carries
	// fallback_index (its zero-based destination stage). A reactive skip
	// additionally carries cooldown (true) and cooldown_until,
	// with attempts=0 and no error.
	//
	// This is the record that a fallback *chain* fired. Proxy / env
	// overrides that rewrite the model without changing backend are
	// EventModelDrift instead; a chain fall-through that also changes
	// the model emits both.
	EventModelFallback EventType = "model_fallback"

	// EventModelDrift is emitted when a backend reports an effective
	// model that is not the workflow-declared `model:` (ignoring a
	// `provider/` routing prefix). The CLI backends already logged this
	// as "requested X resolved to Y"; without a store event the drift
	// was invisible in the studio and unrecoverable from events.jsonl.
	//
	// Distinct from EventModelFallback: drift is "what ran ≠ what was
	// asked", whatever the cause (proxy, ANTHROPIC_MODEL, a fallback
	// route, pi fuzzy-matching a typo). Fallback is specifically a
	// chain fall-through.
	//
	// Data keys: backend, declared_model, effective_model.
	EventModelDrift EventType = "model_drift"

	// EventSandboxSkipped is emitted at run start when the workflow or a
	// node requested an active sandbox mode (auto/inline) but the
	// resolved driver cannot honour it — typically the noop driver on a
	// host without docker, or the cloud V1 fallback where the runner
	// pod is the de-facto sandbox. The Data field carries:
	//   - driver: the driver that handled the request
	//   - mode: the requested mode ("auto" or "inline")
	//   - reason: human-readable explanation
	EventSandboxSkipped EventType = "sandbox_skipped"
	// EventSandboxStarted fires after the active sandbox driver finishes
	// `Start` (container running, postCreate executed). The data block
	// makes the resolved spec visible to operators without parsing
	// `run.log` — useful for diagnosing "ran with the wrong image" or
	// "postCreate didn't fire" symptoms after the fact. Data:
	//   - driver: docker / podman / kubernetes / noop
	//   - mode: auto / inline / none
	//   - source: how the spec was chosen ("workflow sandbox: block",
	//     "(default image: ...)", "(.devcontainer/devcontainer.json)", ...)
	//   - image: resolved image ref
	//   - has_post_create: whether the spec carries a non-empty
	//     postCreateCommand
	EventSandboxStarted EventType = "sandbox_started"
	// EventSandboxWorkspaceExportFailed fires when a copy-based sandbox
	// driver (kubernetes) fails to export the pod workspace back to the
	// host at run end — the run's in-pod commits then stay invisible to
	// the host-side git metadata (Commits/Files panels) and worktree
	// finalization, even though a bot-side `git push` may have already
	// delivered them to the remote. Data:
	//   - driver: the sandbox driver
	//   - error: the export failure
	EventSandboxWorkspaceExportFailed EventType = "sandbox_workspace_export_failed"
	// EventSandboxClawRoutedViaRunner fires when a sandboxed run
	// contains a node using backend=claw — the engine forwards the
	// call to iterion __claw-runner inside the container. Data:
	//   - reason: short summary
	//   - limitations_v1: known V1 caveats
	EventSandboxClawRoutedViaRunner EventType = "sandbox_claw_routed_via_runner"
	// EventSandboxHostStateMounted fires when the runtime auto-binds
	// the host's persistent state directories (`~/.iterion` run store
	// and `~/.claude` Claude Code OAuth/sessions) into the sandbox.
	// Lets operators audit what slipped into the container and spot
	// "host_state was on when I expected off" misconfigurations. Data:
	//   - enabled: whether the auto-mount ran (false = host_state=none)
	//   - source: precedence label (CLI > workflow > env > default)
	//   - mounts: []string of "host_path:container_path" pairs (only
	//     paths actually mounted are listed; skipped ones are absent)
	EventSandboxHostStateMounted EventType = "sandbox_host_state_mounted"
	// EventSandboxUserRemap fires when the docker driver injects
	// `--user $(id -u):$(id -g)` because host_state=auto requires
	// host-UID-owned writes into the mounted ~/.iterion / ~/.claude
	// trees. Data:
	//   - uid: host UID
	//   - gid: host GID
	//   - reason: why the remap was applied
	EventSandboxUserRemap EventType = "sandbox_user_remap"
	// EventSandboxUIDMismatchWarning fires when host_state=auto is
	// active but the spec already pins a User that differs from the
	// host UID — likely a devcontainer.json's remoteUser. We respect
	// the explicit User but warn so operators see why host_state
	// writes may end up root-owned. Data:
	//   - spec_user: the value of Spec.User
	//   - host_uid: host UID (only emitted on Linux hosts)
	EventSandboxUIDMismatchWarning EventType = "sandbox_uid_mismatch_warning"
	// EventSandboxDevboxProvisioned fires when the runtime finds a
	// `devbox.json` for the bot (bundle root) or the target repo
	// (workspace root) and provisions it for the run. Two targets:
	//   - "sandbox": `devbox install` in the container's PostCreate plus
	//     the resulting profile bin dir prepended to the container PATH;
	//   - "host": no sandbox is active (every cloud run — the runner pod
	//     is the isolation boundary — and local no-sandbox runs), so
	//     `devbox install` runs on the executing host and the profile
	//     bin dirs are threaded into every host-spawned command's PATH.
	// Either way, tool nodes — which run a non-interactive `sh -c` that
	// sources no profile — find the packages. Data:
	//   - target: "sandbox" | "host"
	//   - sources: []string of the devbox sources picked up ("repo", "bot"),
	//     in PATH-precedence order
	//   - configs: []string of the host devbox.json paths that triggered it
	//   - bin_dirs: []string of profile bin dirs added to PATH
	//   - path: the resulting PATH
	//   - errors: []string (host target only) — provisioning problems
	//     (devbox binary missing, staging or install failures); the run
	//     proceeds, the named packages are absent
	EventSandboxDevboxProvisioned EventType = "sandbox_devbox_provisioned"
	// EventNetworkBlocked fires every time the iterion CONNECT proxy
	// rejects a request. Data:
	//   - host: blocked hostname
	//   - reason: rule that fired
	//   - run_id: the run scope
	EventNetworkBlocked EventType = "network_blocked"
	// EventPersistDegraded fires when session: persist could not pack or
	// store the CLI session (sandbox without host_state, empty config
	// dir, Put failure). The node succeeded; the next visit runs fresh.
	// Data: reason (short).
	EventPersistDegraded EventType = "persist_session_degraded"
	// EventSessionDegraded is the RESTORE-side twin of
	// EventPersistDegraded: a node whose session is best-effort
	// (`session: inherit_if_available` / `persist`) resolved an id whose
	// backing state no longer loads — a cloud resume replaced the sandbox
	// container and the CLI's session files died with it — so the
	// executor re-ran the call with the session dropped. The node
	// SUCCEEDS having lost its upstream (or accumulated) conversation,
	// which is precisely why it must be loud: without this event the only
	// trace is a process log line, and nothing in the run record would say
	// the node started amnesiac. The node's own output carries the
	// machine-readable half (`_session_degraded`), so a deterministic gate
	// can fail closed on it.
	//
	// Data keys: backend, session_id (the id that failed to serve),
	// reason (delegate.FallbackCategory), error.
	EventSessionDegraded EventType = "session_degraded"
	// EventSandboxBuildStarted fires when the engine calls
	// [sandbox.Builder.Build] between Prepare and Start (V2-6, docker
	// driver via `docker buildx build --load`). Data:
	//   - driver: the driver name (e.g. "docker")
	//   - dockerfile: spec.Build.Dockerfile (relative path)
	//   - context: spec.Build.Context (relative path)
	EventSandboxBuildStarted EventType = "sandbox_build_started"
	// EventSandboxBuildFinished fires when the build completed
	// successfully and the freshly-tagged ref is plumbed into the
	// sibling container's spec.Image. Data:
	//   - driver: the driver name
	//   - target: locally-tagged image ref
	//   - duration_ms: end-to-end build time
	EventSandboxBuildFinished EventType = "sandbox_build_finished"
	// EventSandboxBuildFailed fires when the build tool (e.g. docker
	// buildx) exits non-zero. Data:
	//   - driver: the driver name
	//   - error: short error summary including the last ~4 KB of
	//     stderr (the "ERROR: failed to solve" footer)
	EventSandboxBuildFailed EventType = "sandbox_build_failed"
	// EventPreviewURLAvailable signals that the run has a URL worth
	// rendering in the studio's Browser pane (dev server, deploy preview,
	// HTML artifact). Emitted by the runtime when a tool node prints
	// the convention line `[iterion] preview_url=<url>` on stdout, or
	// directly by the runtime/sandbox when it knows about a forwarded
	// dev-server port. Data:
	//   - url: the URL to render
	//   - kind: optional hint ("dev-server", "deploy", "artifact-html")
	//   - scope: "internal" (route through /api/runs/:id/preview to
	//     strip frame-ancestors / X-Frame-Options) or "external"
	//     (load directly in iframe — only works if the target site
	//     allows embedding). Defaults to "external" when unset.
	//   - source: optional, "tool-stdout" or "runtime"
	EventPreviewURLAvailable EventType = "preview_url_available"
	// EventBrowserScreenshot is emitted whenever the runtime captures
	// a static screenshot of a preview URL — either via the tool-node
	// directive `[iterion] preview_screenshot=<path> [url=<u>]` or,
	// in PR 3, on every Playwright `browser_*` action. The bytes
	// themselves are persisted as a regular attachment (PNG/JPEG via
	// store.WriteAttachment); this event carries only the pointer plus
	// the URL the screenshot is *of* so the studio's scrubber can
	// pick the right artefact for a given seq. Data:
	//   - attachment_name: store.AttachmentRecord.Name
	//   - url: optional, the URL the screenshot represents
	//   - source: "tool-stdout" or "playwright" (PR 3)
	//   - tool_call_id: optional, used by PR 3 to correlate with the
	//     Playwright tool call that produced the frame
	EventBrowserScreenshot EventType = "browser_screenshot"
	// EventRunAttachmentPublished fires when a tool node hands a file
	// it produced to the run via `[iterion] attachment=<path>`. The
	// bytes are persisted as a regular attachment; this event carries
	// only the pointer, so a downstream gate can show the deliverable.
	//
	// Deliberately NOT EventBrowserScreenshot: that one means "an image
	// of a preview URL", and the studio appends it to the Browser
	// pane's screenshot filmstrip. An mp4 or a pdf announced there
	// would render as a broken <img>, open the Browser pane on a run
	// that never opened a browser, and evict real captures from the
	// scrubber. Data:
	//   - attachment_name: store.AttachmentRecord.Name of the file
	//   - mime: the stored (already neutralised) media type
	//   - source: "tool-stdout"
	EventRunAttachmentPublished EventType = "run_attachment_published"
	// EventBrowserSessionStarted fires when the runtime attaches a
	// Chromium instance to a node and registers it in the
	// BrowserRegistry. The studio uses this signal to flip the
	// Browser pane to live mode and dial the CDP WS proxy. Data:
	//   - session_id: BrowserSession.SessionID, also the WS query arg
	//   - node_id: which node the session is bound to
	EventBrowserSessionStarted EventType = "browser_session_started"
	// EventBrowserSessionEnded fires when Detach is called — either
	// because the node finished, the run was cancelled, or the
	// runtime tore the registry down on Manager.Close. The studio
	// closes the CDP WS and falls back to viewer mode. Data:
	//   - session_id: matches the prior _started event
	EventBrowserSessionEnded EventType = "browser_session_ended"
	// EventUserMessageQueued is emitted when an operator enqueues a
	// chat message against a running run via POST /api/runs/{id}/
	// queue-message (or WS queue_message). The engine drains queued
	// messages between agent-loop iterations (claw) or at the next
	// human pause (claude_code / codex). Data carries the
	// QueuedUserMessage record.
	EventUserMessageQueued EventType = "user_message_queued"
	// EventUserMessageDelivered fires when the engine extracts a
	// queued message from the inbox and hands it to the agent. For
	// claw this happens inline at the tool-iteration boundary; for
	// claude_code / codex it happens at the next pauseAtHuman.
	EventUserMessageDelivered EventType = "user_message_delivered"
	// EventUserMessageConsumed fires when the LLM's next response
	// observably incorporates the delivered message (used by the
	// studio inbox to switch the badge from "delivered" to "consumed"
	// and hide the message after a short delay). Heuristic: emitted
	// at the next tool-iteration boundary after delivery.
	EventUserMessageConsumed EventType = "user_message_consumed"
	// EventUserMessageCancelled fires when the operator cancels a
	// queued (not-yet-delivered) message via DELETE /api/runs/{id}/
	// queue-message/{msgId}.
	EventUserMessageCancelled EventType = "user_message_cancelled"
	// EventWorktreeBranchFailed fires when finalizeWorktree could not
	// create the persistent storage branch for a worktree:auto run's
	// commits (validation of a malformed default name, repeated
	// "already exists" on the fallback variants, or git itself
	// erroring). The commits remain reachable via reflog for the
	// repository's default GC window. Data:
	//   - sha: final commit SHA (use `git branch <name> <sha>` to recover)
	//   - reason: short string identifying the failure mode
	//     ("invalid_name", "git_branch_failed")
	//   - branch: attempted branch name (the requested one or the
	//     `iterion/run/<friendly>` default — empty when the attempted
	//     name itself was the rejected input)
	EventWorktreeBranchFailed EventType = "worktree_branch_failed"
)

// Event is a single timestamped fact persisted in events.jsonl.
// The Data field carries event-specific payload; its concrete shape
// depends on Type.
//
// bson tags align with plan §D.2: monotonic per-run seq + ts (Mongo
// time field) + run_id partition key. The Mongo backend assigns _id
// itself (ObjectId), so we don't expose one here.
type Event struct {
	Seq       int64          `json:"seq" bson:"seq"`      // monotonic sequence within the run
	Timestamp time.Time      `json:"timestamp" bson:"ts"` // wall-clock time
	Type      EventType      `json:"type" bson:"type"`
	RunID     string         `json:"run_id" bson:"run_id"`
	BranchID  string         `json:"branch_id,omitempty" bson:"branch_id,omitempty"`
	NodeID    string         `json:"node_id,omitempty" bson:"node_id,omitempty"`
	Data      map[string]any `json:"data,omitempty" bson:"data,omitempty"`
	// TenantID partitions events for change-stream + RBAC filtering.
	// Stamped from ctx at write time in cloud mode; empty for local
	// runs and legacy filesystem events.
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	// LogOffset is the byte position in the run's log buffer at the
	// moment this event was persisted. Stamped by the store from the
	// per-run log buffer's running total (filesystem mode only;
	// cloud-mode runs have no local log buffer so the field stays 0).
	// Consumers (studio time-travel scrubber, replay) slice the live
	// log up to this offset to show "what was logged at the moment
	// this event fired" without parsing log line timestamps.
	LogOffset int64 `json:"log_offset,omitempty" bson:"log_offset,omitempty"`
	// ActiveMs is the run's monotonic active duration (milliseconds
	// since run start) at the moment this event was persisted, sourced
	// from the engine's SharedBudget CLOCK_MONOTONIC clock — NOT from
	// wall-clock event timestamps. This is the engine-authoritative
	// active time: OS-suspend windows are EXCLUDED (the monotonic clock
	// freezes while the machine sleeps) while long LLM thinking is
	// counted, and prior active time is preserved across resume. The
	// runview snapshot reducer treats a non-zero value as the
	// authoritative base for the displayed active_duration_ms, falling
	// back to the (suspend-inflating) wall-clock accumulation only for
	// pre-fix events that carry 0. Stamped by the store when the
	// runview Service / runner wired a callback; 0 when unknown (no
	// callback, or the workflow declares no budget).
	ActiveMs int64 `json:"active_ms,omitempty" bson:"active_ms,omitempty"`
}

// IsAsyncHumanInput reports whether a human_input_requested event marks
// a NON-BLOCKING async question (ADR-081, ask_user_async): the run keeps
// executing, so pause-driven consumers (exec-status reducers, stall
// alerting, pause forms) must skip it. One grep-able helper instead of a
// hand-rolled Data read at every consumer.
func IsAsyncHumanInput(evt Event) bool {
	if evt.Type != EventHumanInputRequested {
		return false
	}
	async, _ := evt.Data[asyncEventDataKey].(bool)
	return async
}
