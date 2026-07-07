// WhatsNextMessage describes one entry in the chat transcript. The
// transcript is a flat ordered list; events from the run lifecycle
// fold into messages via the generic runChat module
// (`@/lib/runChat/messagesFromEvents`) parameterised by a whats-next
// resolver in `messagesFromEvents.ts` (this directory).
//
// The bot-specific types below (RoadmapCardMessage, IssuesSummaryMessage,
// SurveyCardMessage, PlanHandedOffMessage) are produced from generic
// ExtensionMessage entries by the whats-next post-processor — see
// `messagesFromEvents.ts`. The runChat folder itself never knows about
// them.
//
// The shared envelope types (BannerMessage, HumanQuestionMessage,
// SessionClosedMessage, BannerProgress, BannerStatus, QuickActionKind)
// live in `@/lib/runChat/types` and are re-exported here for backward
// compatibility with the WhatsNext components that import them under
// these names.

export type {
  BannerStatus,
  BannerProgress,
  BannerMessage,
  HumanQuestionMessage,
  QuickActionKind,
  SessionClosedMessage,
  UserMessage,
  UserMessageStatus,
} from "@/lib/runChat/types";

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

// One row from the load_dispatch_candidates agent's output. Mirrors
// the schema's `candidates[]` shape; the studio's
// resolveDynamicForm consumes it to populate the ask_which_to_dispatch_more
// checkbox column.
export interface DispatchCandidate {
  id: string;
  title: string;
  assignee?: string;
  state?: string;
  bot?: string;
}

export interface DispatchCandidatesMessage {
  kind: "dispatch-candidates";
  id: string;
  nodeId: string;
  candidates: DispatchCandidate[];
  summary: string;
}

// TriageSummaryMessage carries the structured output of a triage_board
// turn. The board_summary banner text already conveys "what happened"
// to the operator; the structured payload is what powers the
// "dispatch what I just created" shortcut on the next ask_continue
// (resolveDynamicForm reads created_issue_ids to decide whether to
// surface the radio option, and the formatAnswer / contextual prompt
// resolve the IDs back to titles via the issue cache that lives in
// the WatchPanel poll loop).
export interface TriageSummaryMessage {
  kind: "triage-summary";
  id: string;
  nodeId: string;
  boardSummary: string;
  dispatchedIds: string[];
  createdIssueIds: string[];
  needsFollowup: boolean;
}

// SurveyCardMessage carries the structured output of an agent that
// surveyed the workspace (whats-next's `explore` node today).
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

// PlanHandedOffMessage is a milestone marker pushed when emit_action
// completes — independently of run termination. The post-emit triage
// loop keeps the run alive after emit_action; this marker gives the
// operator the visual milestone (green check, count of issues created)
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

import type {
  AssistantTextMessage as _AssistantTextMessage,
  BannerMessage as _BannerMessage,
  HumanQuestionMessage as _HumanQuestionMessage,
  SessionClosedMessage as _SessionClosedMessage,
  UserMessage as _UserMessage,
} from "@/lib/runChat/types";

export type WhatsNextMessage =
  | _BannerMessage
  | _HumanQuestionMessage
  | RoadmapCardMessage
  | IssuesSummaryMessage
  | SurveyCardMessage
  | _SessionClosedMessage
  | PlanHandedOffMessage
  | DispatchCandidatesMessage
  | TriageSummaryMessage
  | _UserMessage
  | _AssistantTextMessage;

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

export function asTriageSummary(value: unknown): {
  boardSummary: string;
  dispatchedIds: string[];
  createdIssueIds: string[];
  needsFollowup: boolean;
} | null {
  if (!value || typeof value !== "object") return null;
  const v = value as Record<string, unknown>;
  const boardSummary = typeof v.board_summary === "string" ? v.board_summary : "";
  const dispatched = Array.isArray(v.dispatched_ids)
    ? (v.dispatched_ids.filter((x): x is string => typeof x === "string") as string[])
    : [];
  const created = Array.isArray(v.created_issue_ids)
    ? (v.created_issue_ids.filter((x): x is string => typeof x === "string") as string[])
    : [];
  const needsFollowup = typeof v.needs_followup === "boolean" ? v.needs_followup : false;
  if (boardSummary === "" && dispatched.length === 0 && created.length === 0) {
    return null;
  }
  return {
    boardSummary,
    dispatchedIds: dispatched,
    createdIssueIds: created,
    needsFollowup,
  };
}

// asDispatchCandidates lifts a load_dispatch_candidates node output
// into the typed shape resolveDynamicForm wants. Returns null when
// the shape doesn't match — the upstream prompt is loose enough
// that an empty board still produces a valid (zero-length) payload.
export function asDispatchCandidates(value: unknown): {
  candidates: DispatchCandidate[];
  summary: string;
} | null {
  if (!value || typeof value !== "object") return null;
  const v = value as Record<string, unknown>;
  const rawCandidates = Array.isArray(v.candidates) ? v.candidates : null;
  if (!rawCandidates) return null;
  const candidates: DispatchCandidate[] = rawCandidates
    .filter(
      (x): x is Record<string, unknown> =>
        !!x && typeof x === "object" && typeof (x as Record<string, unknown>).id === "string",
    )
    .map((x) => {
      const id = x.id as string;
      const title = typeof x.title === "string" ? x.title : id;
      const assignee = typeof x.assignee === "string" ? x.assignee : undefined;
      const state = typeof x.state === "string" ? x.state : undefined;
      const bot = typeof x.bot === "string" ? x.bot : undefined;
      return { id, title, assignee, state, bot };
    });
  const summary = typeof v.summary === "string" ? v.summary : "";
  return { candidates, summary };
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
