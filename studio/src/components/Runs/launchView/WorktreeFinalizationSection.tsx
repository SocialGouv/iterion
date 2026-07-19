// Extracted from LaunchView.tsx to keep that file focused.
// WorktreeFinalizationSection renders the collapsible "Worktree
// finalization" block: merge_into / branch_name / merge_strategy /
// auto_merge inputs. When the workflow doesn't declare `worktree: auto`
// the controls are still rendered (predictable UI) but the toggle
// surfaces a "disabled" Badge — the backend ignores them in that case.

import type { MergeStrategy } from "@/api/runs";

import { forgeLabel } from "@/lib/forge";

import { Badge } from "@/components/ui/Badge";
import { Checkbox } from "@/components/ui/Checkbox";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

import WorktreeTargetSummary from "./WorktreeTargetSummary";

export interface WorktreeFinalizationSectionProps {
  showAdvanced: boolean;
  worktreeOn: boolean;
  mergeInto: string;
  branchName: string;
  mergeStrategy: MergeStrategy;
  autoMerge: boolean;
  onToggle: () => void;
  onMergeIntoChange: (value: string) => void;
  onBranchNameChange: (value: string) => void;
  onMergeStrategyChange: (value: MergeStrategy) => void;
  onAutoMergeChange: (value: boolean) => void;
  /** True in cloud mode: the run happens in an ephemeral runner pod, so
   *  "your current branch" copy is meaningless. The FF/branch semantics
   *  target the cloned target repo's checked-out branch instead. */
  cloud: boolean;
  /** Forge provider of the selected repo target ("github" | "gitlab" |
   *  "forgejo" | …), when known — drives PR/MR wording. Null/undefined
   *  falls back to neutral copy. */
  provider?: string | null;
}

export default function WorktreeFinalizationSection({
  showAdvanced,
  worktreeOn,
  mergeInto,
  branchName,
  mergeStrategy,
  autoMerge,
  onToggle,
  onMergeIntoChange,
  onBranchNameChange,
  onMergeStrategyChange,
  onAutoMergeChange,
  cloud,
  provider,
}: WorktreeFinalizationSectionProps) {
  // "Merge when the run finishes" wording, provider-aware: on a known
  // forge, name the review unit it mirrors ("merge request"-style on
  // GitLab, "pull request"-style elsewhere); neutral otherwise.
  const autoMergeCaption = provider
    ? `${forgeLabel(provider).long}-style auto-merge`
    : "auto-merge";
  return (
    <div className="mt-6 border-t border-border-default pt-4">
      <button
        type="button"
        className="text-xs text-fg-muted hover:text-fg-default flex items-center gap-1"
        onClick={onToggle}
        title={
          worktreeOn
            ? "Configure where the run's commits land."
            : "This workflow doesn't declare `worktree: auto`; the fields below have no effect."
        }
      >
        <span>{showAdvanced ? "▾" : "▸"}</span>
        <span>Worktree finalization (squash / merge)</span>
        {!worktreeOn && (
          <Badge variant="neutral" size="sm">
            disabled
          </Badge>
        )}
      </button>
      {showAdvanced && (
        <div className="mt-3 space-y-3 pl-4 border-l border-border-default">
          {worktreeOn && (
            <WorktreeTargetSummary
              branchName={branchName}
              mergeInto={mergeInto}
              cloud={cloud}
            />
          )}
          <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
            <label htmlFor="launch-merge-into" className="pt-1">
              <div className="text-xs font-medium font-mono">merge_into</div>
              <div className="text-caption text-fg-subtle">
                FF target after run
              </div>
            </label>
            <div>
              <Input
                id="launch-merge-into"
                size="sm"
                type="text"
                className="font-mono"
                placeholder="current (default) | none | <branch-name>"
                value={mergeInto}
                onChange={(e) => onMergeIntoChange(e.target.value)}
              />
              <ul className="mt-1 space-y-0.5 text-caption text-fg-subtle list-disc list-inside">
                <li>
                  Empty / <code>current</code> — fast-forward{" "}
                  {cloud
                    ? "the target repository's default branch checked out in the runner."
                    : "your current branch."}
                </li>
                <li>
                  <code>none</code> — keep commits on the storage branch
                  only.
                </li>
                <li>
                  Named branch — honoured only if it matches the{" "}
                  {cloud ? "runner's" : "your"} checked-out branch.
                </li>
              </ul>
            </div>
          </div>
          <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
            <label htmlFor="launch-branch-name" className="pt-1">
              <div className="text-xs font-medium font-mono">branch_name</div>
              <div className="text-caption text-fg-subtle">Storage branch</div>
            </label>
            <div>
              <Input
                id="launch-branch-name"
                size="sm"
                type="text"
                className="font-mono"
                placeholder="iterion/run/<friendly> (default)"
                value={branchName}
                onChange={(e) => onBranchNameChange(e.target.value)}
              />
              <div className="mt-1 text-caption text-fg-subtle">
                Override the GC-guard branch name. On collision a numeric
                suffix is appended.
              </div>
            </div>
          </div>
          <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
            <label htmlFor="launch-merge-strategy" className="pt-1">
              <div className="text-xs font-medium font-mono">merge_strategy</div>
              <div className="text-caption text-fg-subtle">
                Squash vs merge commit
              </div>
            </label>
            <div>
              <Select
                id="launch-merge-strategy"
                value={mergeStrategy}
                onChange={(e) =>
                  onMergeStrategyChange(e.target.value as MergeStrategy)
                }
              >
                <option value="squash">Squash and merge (default)</option>
                <option value="merge">Merge commit (preserve history)</option>
              </Select>
              <div className="mt-1 text-caption text-fg-subtle">
                Used when the run is merged into the target branch — at
                end of run if auto_merge is on, otherwise from the
                Commits tab. The fast-forward path is used for "merge".
              </div>
            </div>
          </div>
          <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
            <label htmlFor="launch-auto-merge" className="pt-1">
              <div className="text-xs font-medium font-mono">auto_merge</div>
              <div className="text-caption text-fg-subtle">
                {autoMergeCaption}
              </div>
            </label>
            <div className="pt-1">
              <label className="inline-flex items-center gap-2 text-xs">
                <Checkbox
                  id="launch-auto-merge"
                  checked={autoMerge}
                  onChange={(e) => onAutoMergeChange(e.target.checked)}
                />
                <span>Auto-merge when run finishes</span>
              </label>
              <div className="mt-1 text-caption text-fg-subtle">
                Off by default. Commits land on the storage branch; merge
                them from the Commits tab when ready. When on, the engine
                applies <code>merge_strategy</code> at end of run.
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
