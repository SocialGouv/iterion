// PiloteMessage describes one entry in the chat transcript. The
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

export interface SessionClosedMessage {
  kind: "session-closed";
  id: string;
  // "finished" — emit_action returned cleanly.
  // "failed" — run hit Fail node or unrecoverable error.
  // "cancelled" — user/system cancellation.
  reason: "finished" | "failed" | "cancelled";
}

export type PiloteMessage =
  | BannerMessage
  | HumanQuestionMessage
  | RoadmapCardMessage
  | IssuesSummaryMessage
  | SessionClosedMessage;

// Helper for components: extract a roadmap doc from a raw node output
// object. Returns null if the shape doesn't match. Used by the
// events→messages mapper and (in mock mode) by PiloteView.
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
