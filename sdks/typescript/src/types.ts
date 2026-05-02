/**
 * Type definitions for the iterion SDK.
 *
 * These mirror the on-disk JSON shapes documented in
 * `docs/persisted-formats.md` and the `--json` output of the iterion CLI
 * (see `pkg/cli/run.go`, `pkg/cli/resume.go`, `pkg/cli/inspect.go`,
 * `pkg/store/run.go`).
 */

// ---------------------------------------------------------------------------
// Run lifecycle
// ---------------------------------------------------------------------------

export type RunStatus =
  | "running"
  | "paused_waiting_human"
  | "finished"
  | "failed"
  | "failed_resumable"
  | "cancelled";

// ---------------------------------------------------------------------------
// Persisted shapes (run.json, events.jsonl, artifacts/, interactions/)
// ---------------------------------------------------------------------------

/** Top-level metadata persisted in run.json. */
export interface Run {
  format_version: number;
  id: string;
  workflow_name: string;
  workflow_hash?: string;
  file_path?: string;
  status: RunStatus;
  inputs?: Record<string, unknown>;
  created_at: string;
  updated_at: string;
  finished_at?: string;
  error?: string;
  checkpoint?: Checkpoint;
  artifact_index?: Record<string, number>;
  work_dir?: string;
  worktree?: boolean;
}

/** Runtime state captured when the engine pauses, fails, or is cancelled. */
export interface Checkpoint {
  node_id: string;
  interaction_id: string;
  outputs: Record<string, Record<string, unknown>>;
  loop_counters: Record<string, number>;
  round_robin_counters?: Record<string, number>;
  loop_previous_output?: Record<string, Record<string, unknown>>;
  loop_current_output?: Record<string, Record<string, unknown>>;
  artifact_versions: Record<string, number>;
  vars: Record<string, unknown>;
  interaction_questions?: Record<string, unknown>;
  backend_session_id?: string;
  backend_name?: string;
  backend_conversation?: unknown;
  backend_pending_tool_use_id?: string;
  node_attempts?: Record<string, Record<string, number>>;
}

/** Versioned per-node output, persisted under artifacts/<node>/<version>.json. */
export interface Artifact {
  run_id: string;
  node_id: string;
  version: number;
  data: Record<string, unknown>;
  written_at: string;
}

/** Human input/output exchange persisted under interactions/<id>.json. */
export interface Interaction {
  id: string;
  run_id: string;
  node_id: string;
  requested_at: string;
  answered_at?: string | null;
  questions: Record<string, unknown>;
  answers?: Record<string, unknown>;
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

export type EventType =
  | "run_started"
  | "run_resumed"
  | "run_paused"
  | "run_finished"
  | "run_failed"
  | "run_cancelled"
  | "branch_started"
  | "node_started"
  | "node_finished"
  | "llm_request"
  | "llm_retry"
  | "node_recovery"
  | "llm_step_finished"
  | "tool_called"
  | "tool_error"
  | "artifact_written"
  | "human_input_requested"
  | "human_answers_recorded"
  | "join_ready"
  | "edge_selected"
  | "budget_warning"
  | "budget_exceeded"
  // Forward-compatible escape hatch
  | (string & {});

/** A single record in events.jsonl. */
export interface Event {
  seq: number;
  timestamp: string;
  type: EventType;
  run_id: string;
  branch_id?: string;
  node_id?: string;
  data?: Record<string, unknown>;
}

// ---------------------------------------------------------------------------
// Runtime errors
// ---------------------------------------------------------------------------

export type RuntimeErrorCode =
  | "NODE_NOT_FOUND"
  | "NO_OUTGOING_EDGE"
  | "LOOP_EXHAUSTED"
  | "BUDGET_EXCEEDED"
  | "EXECUTION_FAILED"
  | "WORKSPACE_SAFETY"
  | "TIMEOUT"
  | "CANCELLED"
  | "JOIN_FAILED"
  | "RESUME_INVALID"
  | (string & {});

// ---------------------------------------------------------------------------
// Command result shapes
// ---------------------------------------------------------------------------

/** Result of `iterion run --json`. Discriminated by `status`. */
export type RunResult =
  | RunResultFinished
  | RunResultPaused
  | RunResultFailed
  | RunResultCancelled;

export interface RunResultBase {
  run_id: string;
  workflow: string;
  store: string;
}

export interface RunResultFinished extends RunResultBase {
  status: "finished";
}

export interface RunResultPaused extends RunResultBase {
  status: "paused_waiting_human";
  file?: string;
  interaction_id?: string;
  node_id?: string;
  questions?: Record<string, unknown>;
}

export interface RunResultFailed extends RunResultBase {
  status: "failed";
  error: string;
}

export interface RunResultCancelled extends RunResultBase {
  status: "cancelled";
}

/** Result of `iterion resume --json`. */
export type ResumeResult =
  | ResumeResultFinished
  | ResumeResultPaused
  | ResumeResultFailed
  | ResumeResultCancelled;

export interface ResumeResultBase {
  run_id: string;
  workflow: string;
}

export interface ResumeResultFinished extends ResumeResultBase {
  status: "finished";
}

export interface ResumeResultPaused extends ResumeResultBase {
  status: "paused_waiting_human";
  interaction_id?: string;
  node_id?: string;
  questions?: Record<string, unknown>;
}

export interface ResumeResultFailed extends ResumeResultBase {
  status: "failed";
  error: string;
}

export interface ResumeResultCancelled extends ResumeResultBase {
  status: "cancelled";
}

/**
 * Result of `iterion inspect --json`.
 * - When `runId` is set: `{ run, events?, interactions? }`.
 * - When `runId` is omitted: an array of `Run` objects.
 */
export type InspectResult = InspectSingleResult | Run[];

export interface InspectSingleResult {
  run: Run;
  events?: Event[];
  interactions?: Interaction[];
}

/**
 * Result of `iterion validate --json`. Mirrors the JSON envelope emitted
 * by `pkg/cli/validate.go` (`ValidateResult` Go struct).
 *
 * Note: when validation FAILS the CLI exits non-zero and (today) writes a
 * second `{"error":"validation failed"}` JSON object to stdout from the
 * outer error handler. The SDK's validate() parses the FIRST JSON object,
 * which is always the structured envelope below.
 */
export interface ValidateResult {
  /** Path that was validated (echoed by the CLI). */
  file: string;
  /** Aggregate validity: false when any parse or compile diagnostic is severity=error. */
  valid: boolean;
  /** Compiled workflow name (absent when parsing failed catastrophically). */
  workflow_name?: string;
  /** Number of compiled IR nodes (absent on parse failure). */
  node_count?: number;
  /** Number of compiled IR edges (absent on parse failure). */
  edge_count?: number;
  /** Lexer/parser diagnostics, formatted as `<file>:<line>:<col>: error [<code>]: <msg>`. */
  parse_diagnostics?: string[];
  /** IR-compile and MCP-prepare diagnostics, same string format as parse_diagnostics. */
  compile_diagnostics?: string[];
  /** Forward-compat escape hatch for any future fields. */
  [key: string]: unknown;
}

/**
 * Structured diagnostic shape. The CLI currently emits diagnostics as
 * pre-formatted strings (see ValidateResult.parse_diagnostics /
 * .compile_diagnostics); this struct is reserved for future structured
 * diagnostic payloads.
 */
export interface Diagnostic {
  code?: string;
  severity?: "error" | "warning" | "info" | string;
  message: string;
  node_id?: string;
  hint?: string;
  [key: string]: unknown;
}

/** Result of `iterion diagram --json`. */
export interface DiagramResult {
  /** Source .iter file. */
  file?: string;
  /** Compiled workflow name. */
  workflow_name?: string;
  /** Diagram view: "compact" | "detailed" | "full". */
  view?: string;
  /** Mermaid diagram source. */
  mermaid?: string;
  [key: string]: unknown;
}

/** Result of `iterion report --json`. */
export interface ReportResult {
  run_id?: string;
  /**
   * Path passed via `--output` to the CLI. NOTE: in `--json` mode the CLI
   * always emits the report to stdout and ignores `--output`; this field
   * is populated by the SDK from the call options as a fallback when the
   * CLI emits no stdout.
   */
  output?: string;
  [key: string]: unknown;
}

/** Result of `iterion version`. The CLI prints a plain string. */
export interface VersionResult {
  version: string;
}

// ---------------------------------------------------------------------------
// Generic helpers
// ---------------------------------------------------------------------------

export type JsonValue =
  | string
  | number
  | boolean
  | null
  | JsonValue[]
  | { [key: string]: JsonValue };
