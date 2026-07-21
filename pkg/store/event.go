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
	EventRunResumed           EventType = "run_resumed"
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
	EventRunFinished    EventType = "run_finished"
	EventRunFailed      EventType = "run_failed"
	EventRunCancelled   EventType = "run_cancelled"
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
	EventRunInterrupted   EventType = "run_interrupted"
	EventDelegateStarted  EventType = "delegate_started"
	EventDelegateFinished EventType = "delegate_finished"
	EventDelegateError    EventType = "delegate_error"
	EventDelegateRetry    EventType = "delegate_retry"

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
