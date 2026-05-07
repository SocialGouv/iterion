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
	EventRunStarted           EventType = "run_started"
	EventBranchStarted        EventType = "branch_started"
	EventBranchFinished       EventType = "branch_finished"
	EventNodeStarted          EventType = "node_started"
	EventLLMRequest           EventType = "llm_request"
	EventLLMPrompt            EventType = "llm_prompt"
	EventLLMRetry             EventType = "llm_retry"
	EventNodeRecovery         EventType = "node_recovery"
	EventLLMStepFinished      EventType = "llm_step_finished"
	EventLLMCompacted         EventType = "llm_compacted"
	EventToolCalled           EventType = "tool_called"
	EventToolError            EventType = "tool_error"
	EventArtifactWritten      EventType = "artifact_written"
	EventHumanInputRequested  EventType = "human_input_requested"
	EventRunPaused            EventType = "run_paused"
	EventHumanAnswersRecorded EventType = "human_answers_recorded"
	EventRunResumed           EventType = "run_resumed"
	EventJoinReady            EventType = "join_ready"
	EventNodeFinished         EventType = "node_finished"
	EventEdgeSelected         EventType = "edge_selected"
	EventBudgetWarning        EventType = "budget_warning"
	EventBudgetExceeded       EventType = "budget_exceeded"
	EventRunFinished          EventType = "run_finished"
	EventRunFailed            EventType = "run_failed"
	EventRunCancelled         EventType = "run_cancelled"
	// EventRunInterrupted is emitted when the editor server drains in-flight
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
	// EventSandboxClawRoutedViaRunner fires when a sandboxed run
	// contains a node using backend=claw — the engine forwards the
	// call to iterion __claw-runner inside the container. Data:
	//   - reason: short summary
	//   - limitations_v1: known V1 caveats
	EventSandboxClawRoutedViaRunner EventType = "sandbox_claw_routed_via_runner"
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
	// rendering in the editor's Browser pane (dev server, deploy preview,
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
	// the URL the screenshot is *of* so the editor's scrubber can
	// pick the right artefact for a given seq. Data:
	//   - attachment_name: store.AttachmentRecord.Name
	//   - url: optional, the URL the screenshot represents
	//   - source: "tool-stdout" or "playwright" (PR 3)
	//   - tool_call_id: optional, used by PR 3 to correlate with the
	//     Playwright tool call that produced the frame
	EventBrowserScreenshot EventType = "browser_screenshot"
	// EventBrowserSessionStarted fires when the runtime attaches a
	// Chromium instance to a node and registers it in the
	// BrowserRegistry. The editor uses this signal to flip the
	// Browser pane to live mode and dial the CDP WS proxy. Data:
	//   - session_id: BrowserSession.SessionID, also the WS query arg
	//   - node_id: which node the session is bound to
	EventBrowserSessionStarted EventType = "browser_session_started"
	// EventBrowserSessionEnded fires when Detach is called — either
	// because the node finished, the run was cancelled, or the
	// runtime tore the registry down on Manager.Close. The editor
	// closes the CDP WS and falls back to viewer mode. Data:
	//   - session_id: matches the prior _started event
	EventBrowserSessionEnded EventType = "browser_session_ended"
)

// Event is a single timestamped fact persisted in events.jsonl.
// The Data field carries event-specific payload; its concrete shape
// depends on Type.
//
// bson tags align with plan §D.2: monotonic per-run seq + ts (Mongo
// time field) + run_id partition key. The Mongo backend assigns _id
// itself (ObjectId), so we don't expose one here.
type Event struct {
	Seq       int64                  `json:"seq" bson:"seq"`      // monotonic sequence within the run
	Timestamp time.Time              `json:"timestamp" bson:"ts"` // wall-clock time
	Type      EventType              `json:"type" bson:"type"`
	RunID     string                 `json:"run_id" bson:"run_id"`
	BranchID  string                 `json:"branch_id,omitempty" bson:"branch_id,omitempty"`
	NodeID    string                 `json:"node_id,omitempty" bson:"node_id,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty" bson:"data,omitempty"`
	// TenantID partitions events for change-stream + RBAC filtering.
	// Stamped from ctx at write time in cloud mode; empty for local
	// runs and legacy filesystem events.
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
}
