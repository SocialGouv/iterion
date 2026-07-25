// Planner provenance formatting.
//
// The board does NOT group parents and children: every card — planner or
// spawned — is an ordinary, independently-ordered cell, and a parent lands
// in Closed as soon as its own run finishes. The relation survives purely
// as data on the two faces: the closed-children counter on the parent
// (these helpers), and the parent name above each child's title.
//
// (An earlier iteration folded children into an expand/collapse campaign
// block. It was removed: a full-width block broke the grid, and holding an
// executed parent in Opened made its lane lie about its run.)

import type { PipelineBoardCard } from "@/api/pipelineBoards";

/** Board-closed children = success (done) + failed/cancelled. */
export function childrenClosedCount(
  s: PipelineBoardCard["children_summary"] | undefined,
): number {
  if (!s) return 0;
  return (s.done ?? 0) + (s.failed ?? 0);
}

/** e.g. "2/5 closed" — primary campaign progress label. */
export function formatChildrenClosedRatio(
  s: PipelineBoardCard["children_summary"] | undefined,
  fallbackTotal?: number,
): string {
  if (s && s.total > 0) {
    return `${childrenClosedCount(s)}/${s.total} closed`;
  }
  if (fallbackTotal && fallbackTotal > 0) {
    return `0/${fallbackTotal} closed`;
  }
  return "";
}

export function formatChildrenSummary(
  s: PipelineBoardCard["children_summary"] | undefined,
  fallbackCount?: number,
): string {
  if (!s) {
    return fallbackCount && fallbackCount > 0
      ? `${fallbackCount} children`
      : "";
  }
  const closed = childrenClosedCount(s);
  const parts: string[] = [`${closed}/${s.total} closed`];
  if (s.in_progress) parts.push(`${s.in_progress} live`);
  if (s.ready) parts.push(`${s.ready} ready`);
  if (s.open) parts.push(`${s.open} open`);
  return parts.join(" · ");
}

export function childrenProgressPct(
  s: PipelineBoardCard["children_summary"] | undefined,
): number {
  if (!s || s.total <= 0) return 0;
  return Math.min(100, Math.round((childrenClosedCount(s) / s.total) * 100));
}
