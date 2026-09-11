import type { RunStatus, RunSummary } from "@/api/runs";
import { readStringFlag, writeStringFlag } from "@/lib/localStorageFlag";

import type { Conversation } from "./conversations";

export const STUDIO_CHAT_CLIENT_KEY = "iterion.chatDock.clientId";
export const STUDIO_CHAT_GC_GRACE_MS = 60_000;

const REAPABLE_STATUSES = new Set<RunStatus>([
  "queued",
  "running",
  "paused_waiting_human",
  "paused_operator",
  "failed_resumable",
]);

export function readOrCreateStudioChatClientId(): string {
  const existing = readStringFlag(STUDIO_CHAT_CLIENT_KEY, "");
  if (existing) return existing;
  const id =
    typeof crypto !== "undefined" && "randomUUID" in crypto
      ? crypto.randomUUID()
      : `client-${Math.random().toString(36).slice(2)}-${Date.now()}`;
  writeStringFlag(STUDIO_CHAT_CLIENT_KEY, id);
  return id;
}

export interface StudioChatReconciliationPlan {
  repairs: Array<{ conversationId: string; runId: string }>;
  cancelRunIds: string[];
  /** Delay before one bounded follow-up scan; null when no young candidate was skipped. */
  retryAfterMs: number | null;
}

/**
 * Reconcile only runs atomically stamped by THIS browser profile's dock.
 * Workflow names are intentionally irrelevant: a manual Copi/Nexie run is
 * not owned by the tab strip and must never be cancelled here.
 */
export function planStudioChatReconciliation(
  runs: readonly RunSummary[],
  conversations: readonly Conversation[],
  clientId: string,
  nowMs: number,
  graceMs = STUDIO_CHAT_GC_GRACE_MS,
): StudioChatReconciliationPlan {
  const candidates = runs.filter(
    (run) =>
      run.source?.kind === "studio_chat" &&
      run.source.client_id === clientId &&
      !!run.source.conversation_id,
  );
  const tabs = new Map(conversations.map((conversation) => [conversation.id, conversation]));
  const byConversation = new Map<string, RunSummary[]>();
  for (const run of candidates) {
    const conversationId = run.source?.conversation_id;
    if (!conversationId) continue;
    const group = byConversation.get(conversationId) ?? [];
    group.push(run);
    byConversation.set(conversationId, group);
  }

  const repairs: StudioChatReconciliationPlan["repairs"] = [];
  const cancelRunIds: string[] = [];
  let retryAfterMs: number | null = null;

  for (const [conversationId, group] of byConversation) {
    group.sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
    const tab = tabs.get(conversationId);
    let keepRunId: string | null = null;
    if (tab?.runId) {
      // The explicit tab owner wins. If it points outside this group, every
      // source-stamped run in the group is a duplicate/orphan.
      keepRunId = group.some((run) => run.id === tab.runId) ? tab.runId : null;
    } else if (tab && group[0]) {
      keepRunId = group[0].id;
      repairs.push({ conversationId, runId: keepRunId });
    }

    for (const run of group) {
      if (run.id === keepRunId) continue;
      // Terminal history needs no cancellation and is never deleted by this
      // reconciliation. It still participates above so a tab that lost its
      // runId after createRun can recover even if the run ended quickly.
      if (!REAPABLE_STATUSES.has(run.status)) continue;
      const createdMs = Date.parse(run.created_at);
      const ageMs = Number.isFinite(createdMs)
        ? Math.max(0, nowMs - createdMs)
        : graceMs;
      if (ageMs >= graceMs) {
        cancelRunIds.push(run.id);
      } else {
        const remaining = graceMs - ageMs;
        retryAfterMs = retryAfterMs === null ? remaining : Math.min(retryAfterMs, remaining);
      }
    }
  }

  return { repairs, cancelRunIds, retryAfterMs };
}

export const STUDIO_CHAT_REAPABLE_STATUSES = Array.from(REAPABLE_STATUSES);
