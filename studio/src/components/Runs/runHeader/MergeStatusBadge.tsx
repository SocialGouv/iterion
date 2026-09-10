// Extracted from RunHeader.tsx to keep that file focused.
// Per-merge-status pill shown inside FinalizationRow (merged/pending/
// failed/conflicted/skipped + legacy fallbacks).

import type { RunHeader as RunHeaderType } from "@/api/runs";

export interface MergeStatusBadgeProps {
  status: RunHeaderType["merge_status"];
  strategy: RunHeaderType["merge_strategy"];
  merged: string;
  mergedShort: string;
  branch: string;
}

export default function MergeStatusBadge({
  status,
  strategy,
  merged,
  mergedShort,
  branch,
}: MergeStatusBadgeProps) {
  if (status === "merged" && merged) {
    return (
      <span className="ml-2 px-1.5 py-0.5 rounded bg-success-soft text-success-fg">
        {strategy === "squash" ? "squashed" : "merged"} into {merged}
        {mergedShort && (
          <span className="ml-1 font-mono text-success-fg/80">
            · {mergedShort}
          </span>
        )}
      </span>
    );
  }
  if (status === "pending") {
    return (
      <span className="ml-2 px-1.5 py-0.5 rounded bg-info-soft text-info-fg">
        awaiting merge — open Commits tab
      </span>
    );
  }
  if (status === "failed") {
    return (
      <span
        className="ml-2 px-1.5 py-0.5 rounded bg-danger-soft text-danger-fg"
        title="Open the left-panel Commits tab to retry."
      >
        merge failed — retry from Commits tab
      </span>
    );
  }
  if (status === "merging") {
    return (
      <span
        className="ml-2 px-1.5 py-0.5 rounded bg-info-soft text-info-fg"
        title="A worker holds the merge claim; the record updates when it completes."
      >
        merge in progress…
      </span>
    );
  }
  if (status === "conflicted") {
    return (
      <span
        className="ml-2 px-1.5 py-0.5 rounded bg-warning-soft text-warning-fg"
        title="Open the Commits tab to resolve the conflict."
      >
        merge conflict — resolve in Commits tab
      </span>
    );
  }
  if (status === "skipped") {
    return (
      <span className="ml-2 text-fg-subtle">
        merge skipped — branch{" "}
        <code className="text-fg-default">{branch}</code> preserved
      </span>
    );
  }
  // Legacy runs (pre-merge_status) that recorded merged_into without a
  // status field. Keep the old wording.
  if (merged) {
    return (
      <span className="ml-2 px-1.5 py-0.5 rounded bg-success-soft text-success-fg">
        merged into {merged} ✓
      </span>
    );
  }
  if (branch) {
    return (
      <span className="ml-2 text-fg-subtle">
        not auto-merged — run{" "}
        <code className="text-fg-default">git merge {branch}</code>
      </span>
    );
  }
  return null;
}
