// WhatsNextMessage describes one entry in the chat transcript. The
// transcript is a flat ordered list; events from the run lifecycle
// fold into messages via `messagesFromEvents` (added in Étape 2).
//
// The discriminated union mirrors the node kinds in
// `firstClassBots.ts` plus a few synthetic entries (user reply,
// session-closed marker) that aren't directly tied to a node.

export type BannerStatus = "running" | "done" | "failed";

// Live progress accumulated while a banner is "running". Drives the
// "12 tools used · latest: bash" line under the spinner so an explore
// or propose_roadmap pass doesn't look frozen during the 30–60s the
// agent actually needs.
export interface BannerProgress {
  // Number of `tool_started` events seen on this node's active iter.
  toolCount: number;
  // Name of the most recent tool the node invoked (e.g. "bash",
  // "read_file"). Used as a "doing X" hint.
  latestTool?: string;
  // Brief textual hint extracted from the latest tool's input — the
  // first argument-ish field, when present. Surfaces "read_file: README.md"
  // rather than just "read_file".
  latestToolHint?: string;
}

export interface BannerMessage {
  kind: "banner";
  id: string; // stable: "<nodeId>:<iteration>" (iteration so revise loops don't collide)
  nodeId: string;
  label: string;
  status: BannerStatus;
  // When status === "done", the optional one-line summary plucked
  // from the node output (per nodeMap.summaryField).
  summary?: string;
  errorMessage?: string;
  // Live counters updated while status === "running".
  progress?: BannerProgress;
}

// Quick-action markers the operator can emit instead of typing a
// reply. The bot's prompt is expected to recognise these tokens and
// react accordingly ([QA:skip] proceed empty, [QA:idk] fall back to
// best inference, [QA:later] defer if the flow has a re-ask point).
// "later" is meaningful only inside the new triage loop where the
// bot can re-ask; outside it's effectively a "skip".
export type QuickActionKind = "skip" | "idk" | "later";

export interface HumanQuestionMessage {
  kind: "human-question";
  id: string;
  nodeId: string;
  prompt: string;
  // Pending = awaiting user; answered = past tour in the loop.
  status: "pending" | "answered";
  // The verbatim text the user typed when status === "answered".
  // For approve/reject actions (human_review), this is the inline
  // feedback (may be empty when approved without comment).
  userReply?: string;
  // Optional structured outcome (e.g. { approved: true }).
  outcome?: Record<string, unknown>;
  // For human_review etc., which action buttons to render.
  actions?: ReadonlyArray<"approve" | "request_revision">;
  // Runtime-resolved questions payload (the same data the engine
  // writes into checkpoint.InteractionQuestions). Carries field
  // schema / labels / hints the bot or LLM produced — set when the
  // `human_input_requested` event arrived with a `questions` map.
  // The form renderer falls back to the bot's static `prompt` when
  // this is empty.
  questions?: Record<string, unknown>;
  // Inline shortcuts the operator can pick instead of typing a
  // reply. Only rendered on free-text turns (the form / action
  // paths already give the operator structured choices). Default
  // for free-text turns: ["skip", "idk"]. Pass an empty array to
  // suppress. The runtime can override per-node via a future DSL
  // `quick_actions:` block; for now the default is client-side.
  quickActions?: ReadonlyArray<QuickActionKind>;
}

export interface RoadmapItem {
  title: string;
  body: string;
  assignee: string;
  args?: Record<string, unknown>;
}

export interface RoadmapDoc {
  long_term: RoadmapItem[];
  short_term: RoadmapItem[];
  next_action: RoadmapItem | null;
  rationale: string;
}

export interface RoadmapCardMessage {
  kind: "roadmap-card";
  id: string;
  nodeId: string;
  iteration: number; // 0 = propose, 1+ = revision n
  roadmap: RoadmapDoc;
}

export interface CreatedIssueRef {
  id: string;
  title: string;
  horizon: "long_term" | "short_term" | "next_action";
  assignee: string;
}

export interface FailedIssueRef {
  title: string;
  horizon: string;
  error: string;
}

export interface IssuesSummaryMessage {
  kind: "issues-summary";
  id: string;
  nodeId: string;
  createdIssues: CreatedIssueRef[];
  failedIssues: FailedIssueRef[];
  planPath: string;
  summary: string;
}

// SurveyCardMessage carries the structured output of an agent that
// surveyed the workspace (whats-next's `explore` node today). The
// matching nodeMap entry sets `followCardKind: "survey"` so the
// runtime mapper pushes one of these right after the banner closes.
export interface SurveyCardMessage {
  kind: "survey-card";
  id: string;
  nodeId: string;
  summary: string;
  openQuestions: string[];
  observations: string;
  // Free-form maps. Surfaced under "Show details" — we don't try to
  // schematise them since their shape varies by repo.
  toplevelDirs: unknown;
  recentCommits: unknown;
}

export interface SessionClosedMessage {
  kind: "session-closed";
  id: string;
  // "finished" — run reached Done (operator picked action=done in
  //   the triage loop, or the bot has no triage loop and reached
  //   its terminal node naturally).
  // "failed" — run hit Fail node or unrecoverable error.
  // "cancelled" — user/system cancellation.
  reason: "finished" | "failed" | "cancelled";
}

// PlanHandedOffMessage is a milestone marker pushed when emit_action
// completes — independently of run termination. The original
// whats-next flow ended at emit_action and used the SessionClosed
// "finished" marker for "plan handed off". With the post-emit triage
// loop, emit_action no longer ends the run; the operator continues
// in the chat to dispatch / refine tickets. This message gives them
// the same visual milestone (green check, count of issues created)
// while the chat stays interactive.
export interface PlanHandedOffMessage {
  kind: "plan-handed-off";
  id: string;
  // Absolute path to the audit markdown emit_action wrote.
  planPath: string;
  // How many kanban issues emit_action created on this turn.
  createdCount: number;
  // Optional one-line summary verbatim from emit_action.summary.
  summary?: string;
}

export type WhatsNextMessage =
  | BannerMessage
  | HumanQuestionMessage
  | RoadmapCardMessage
  | IssuesSummaryMessage
  | SurveyCardMessage
  | SessionClosedMessage
  | PlanHandedOffMessage;

// Helper for components: extract a roadmap doc from a raw node output
// object. Returns null if the shape doesn't match. Used by the
// events→messages mapper and (in mock mode) by WhatsNextView.
export function asRoadmapDoc(value: unknown): RoadmapDoc | null {
  if (!value || typeof value !== "object") return null;
  const v = value as Record<string, unknown>;
  const long_term = Array.isArray(v.long_term) ? (v.long_term as RoadmapItem[]) : [];
  const short_term = Array.isArray(v.short_term) ? (v.short_term as RoadmapItem[]) : [];
  const next_action =
    v.next_action && typeof v.next_action === "object"
      ? (v.next_action as RoadmapItem)
      : null;
  const rationale = typeof v.rationale === "string" ? v.rationale : "";
  // Sanity: at least one of the horizons must be non-empty or rationale set.
  if (
    long_term.length === 0 &&
    short_term.length === 0 &&
    next_action === null &&
    rationale === ""
  ) {
    return null;
  }
  return { long_term, short_term, next_action, rationale };
}

export function asSurveyOutput(value: unknown): {
  summary: string;
  openQuestions: string[];
  observations: string;
  toplevelDirs: unknown;
  recentCommits: unknown;
} | null {
  if (!value || typeof value !== "object") return null;
  const v = value as Record<string, unknown>;
  const summary = typeof v.summary === "string" ? v.summary : "";
  const observations =
    typeof v.observations === "string" ? v.observations : "";
  const openQuestions = Array.isArray(v.open_questions)
    ? (v.open_questions.filter((q): q is string => typeof q === "string") as string[])
    : [];
  if (
    summary === "" &&
    observations === "" &&
    openQuestions.length === 0 &&
    v.toplevel_dirs === undefined &&
    v.recent_commits === undefined
  ) {
    return null;
  }
  return {
    summary,
    openQuestions,
    observations,
    toplevelDirs: v.toplevel_dirs,
    recentCommits: v.recent_commits,
  };
}

export function asEmitOutput(value: unknown): {
  createdIssues: CreatedIssueRef[];
  failedIssues: FailedIssueRef[];
  planPath: string;
  summary: string;
} | null {
  if (!value || typeof value !== "object") return null;
  const v = value as Record<string, unknown>;
  const created = Array.isArray(v.created_issues)
    ? (v.created_issues.filter(
        (x): x is CreatedIssueRef =>
          !!x &&
          typeof x === "object" &&
          typeof (x as CreatedIssueRef).id === "string" &&
          typeof (x as CreatedIssueRef).title === "string",
      ) as CreatedIssueRef[])
    : [];
  const failed = Array.isArray(v.failed_issues)
    ? (v.failed_issues.filter(
        (x): x is FailedIssueRef =>
          !!x &&
          typeof x === "object" &&
          typeof (x as FailedIssueRef).title === "string",
      ) as FailedIssueRef[])
    : [];
  const planPath = typeof v.plan_path === "string" ? v.plan_path : "";
  const summary = typeof v.summary === "string" ? v.summary : "";
  if (created.length === 0 && failed.length === 0 && !planPath && !summary) {
    return null;
  }
  return { createdIssues: created, failedIssues: failed, planPath, summary };
}
