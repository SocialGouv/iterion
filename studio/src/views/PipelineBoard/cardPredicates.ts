// Shared card predicates + comparators for the /pipelines board.
//
// This module exists because the same two questions — "is this card blocked
// by a dependency?" and "which card would the server launch first?" — were
// each answered by several divergent copies scattered across the view. The
// copies drifted (one omitted `blocker_labels`, so a label-gated ticket was
// badged Blocked yet slipped through the filter; another compared
// `undefined` priority against `0` and silently skipped its date tie-break).
// Every consumer now imports from here. Keep it dependency-free and pure so
// it can be unit-tested and reused by badges without a circular import back
// into PipelineColumns.

import type { PipelineBoardCard } from "@/api/pipelineBoards";

/**
 * The lanes this SPA build knows about. Used as a forward-compatibility
 * guard: a card whose column_id is absent from this set comes from a NEWER
 * server that grew a lane we cannot render, and must still be reachable
 * somewhere rather than vanishing from every section (which is exactly what
 * happened to `needs_attention` cards on builds predating that lane).
 */
export const KNOWN_PIPELINE_LANES: ReadonlySet<string> = new Set([
  "opened",
  "in_progress",
  "needs_attention",
  "closed",
]);

export function isKnownLane(columnId: string | undefined): boolean {
  return KNOWN_PIPELINE_LANES.has(columnId ?? "");
}

/**
 * cardBlocked reports that a HARD DEPENDENCY is what stands between this
 * card and a launch — open blockers, a `waiting_deps` parking state, or
 * blocker labels the policy still requires.
 *
 * Deliberately narrower than "the server would refuse to launch this": the
 * other launch_blocked_reason values (`no_bot`, `not_ready`, `missing_issue`)
 * are about the ticket's own preparation, not about waiting on someone else.
 * Demoting an unprepared draft below a dependency-blocked ticket in the
 * Opened sort would be noise — the operator can act on a draft right now.
 * Note also that the server deliberately suppresses `not_ready` for
 * non-staged tickets (attachDeps, pkg/server/pipeline_boards_provenance.go),
 * so it is not a reliable signal here anyway.
 */
export function cardBlocked(card: PipelineBoardCard): boolean {
  if ((card.open_blocker_count ?? 0) > 0) return true;
  if (card.issue_state === "waiting_deps") return true;
  const reason = card.launch_blocked_reason;
  return (
    reason === "open_blockers" ||
    reason === "waiting_deps" ||
    reason === "blocker_labels"
  );
}

/**
 * cardReady reports whether an Opened card is cleared to advance to In
 * progress: staged (server `ready` = StateReady) or already queued for a
 * concurrency slot, AND its hard deps are satisfied. Drives the Ready badge
 * and the Opened tab's readiness chips.
 */
export function cardReady(card: PipelineBoardCard): boolean {
  if (cardBlocked(card)) return false;
  return card.ready === true || card.status === "queued";
}

/**
 * closedOutcome splits the Closed lane. Since the needs-attention lane took
 * over mid-flight failures, `failed === true` in Closed means specifically
 * "the operator stopped it" — the chip is labelled Stopped, not Failed.
 */
export function closedOutcome(card: PipelineBoardCard): "success" | "failed" {
  return card.failed === true ? "failed" : "success";
}

/**
 * compareLaunchOrder mirrors the server's launch order — priority desc, then
 * oldest-created first, then id for stability. Keep in sync with Go's
 * lessIssueByPriorityThenAge (pkg/server/pipeline_admission.go); the shared
 * fixture in filters.test.ts is the TS half of that contract.
 *
 * Priorities are normalized BEFORE the comparison: an absent priority and an
 * explicit 0 are the same rank, and treating them as different used to
 * short-circuit the whole comparator to 0 and lose the date tie-break.
 */
export function compareLaunchOrder(
  a: PipelineBoardCard,
  b: PipelineBoardCard,
): number {
  const pa = a.priority ?? 0;
  const pb = b.priority ?? 0;
  if (pa !== pb) return pb - pa;
  const ta = Date.parse(a.created_at || "") || 0;
  const tb = Date.parse(b.created_at || "") || 0;
  if (ta !== tb) return ta - tb;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

/**
 * compareBlockedLast sinks dependency-blocked cards below launchable ones.
 * Applied BEFORE compareLaunchOrder in the Opened inventory so the top of
 * the list is always something the operator can actually start.
 *
 * This makes the UI agree with reality rather than inventing a preference:
 * the admission loop filters blocked tickets out (native.LaunchBlockedReason)
 * *before* sorting the rest, so a blocked P9 above an unblocked P1 was the
 * list lying about which pipeline goes next.
 */
export function compareBlockedLast(
  a: PipelineBoardCard,
  b: PipelineBoardCard,
): number {
  const ba = cardBlocked(a) ? 1 : 0;
  const bb = cardBlocked(b) ? 1 : 0;
  return ba - bb;
}
