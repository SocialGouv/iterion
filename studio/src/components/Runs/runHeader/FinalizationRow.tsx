// Extracted from RunHeader.tsx to keep that file focused.
// Worktree-finalization summary row (commit SHA, branch, merge status)
// shown under the main RunHeader bar when run.final_commit is set.

import type { RunHeader as RunHeaderType } from "@/api/runs";
import { useCopyTimer } from "@/hooks/useCopyTimer";
import { useUIStore } from "@/store/ui";

import MergeStatusBadge from "./MergeStatusBadge";

// FinalizationRow surfaces the worktree-finalization outcome (commit
// SHA, storage branch, merge target) under the main header bar so the
// user can see at a glance whether the run's commits made it back to
// their branch — and what to do if they didn't. Only rendered when
// final_commit is set (i.e. the run produced commits in its worktree).
export default function FinalizationRow({ run }: { run: RunHeaderType }) {
  const { copied, trigger: triggerCopied } = useCopyTimer<string>(1500);
  const shortSha = (run.final_commit ?? "").slice(0, 7);
  const branch = run.final_branch ?? "";
  const merged = run.merged_into ?? "";
  const status = run.merge_status;
  const strategy = run.merge_strategy;
  const mergedShort = (run.merged_commit ?? "").slice(0, 7);

  const copy = async (text: string, key: string) => {
    try {
      await navigator.clipboard.writeText(text);
      triggerCopied(key);
    } catch {
      // navigator.clipboard is unavailable in insecure contexts (e.g.
      // dev served over plain http without the bypass flag). Surface
      // the failure rather than swallow it so the operator knows the
      // SHA didn't reach their paste buffer.
      useUIStore.getState().addToast(
        "Copy unavailable in this context",
        "warning",
      );
    }
  };

  return (
    <div className="shrink-0 px-4 py-1.5 bg-surface-2/40 border-b border-border-default flex items-center gap-3 text-micro flex-wrap">
      <span className="text-fg-muted">commit</span>
      <button
        type="button"
        className="font-mono text-fg-default hover:text-info focus-visible:ring-1 focus-visible:ring-accent rounded"
        onClick={() => void copy(run.final_commit ?? "", "sha")}
        title="Copy full SHA"
      >
        {shortSha}
        {copied === "sha" && <span className="ml-1 text-fg-subtle">copied</span>}
      </button>
      {branch && (
        <>
          <span className="text-fg-subtle">on</span>
          <button
            type="button"
            className="font-mono text-fg-default hover:text-info focus-visible:ring-1 focus-visible:ring-accent rounded truncate max-w-xs"
            onClick={() => void copy(branch, "branch")}
            title="Copy branch name"
          >
            {branch}
            {copied === "branch" && (
              <span className="ml-1 text-fg-subtle">copied</span>
            )}
          </button>
        </>
      )}
      <MergeStatusBadge
        status={status}
        strategy={strategy}
        merged={merged}
        mergedShort={mergedShort}
        branch={branch}
      />
      {run.loc_added != null && run.loc_deleted != null && (
        <span
          className="font-mono"
          title="Lines changed by the run's commits (three-dot diff against the fork point)"
        >
          <span className="text-success">+{run.loc_added}</span>{" "}
          <span className="text-danger">−{run.loc_deleted}</span>
        </span>
      )}
    </div>
  );
}
