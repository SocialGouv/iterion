// Extracted from LaunchView.tsx to keep that file focused.
// The single Advanced disclosure: everything below the primary inputs is
// tuning, not launch-blocking — optional/auto-managed inputs, backend,
// per-node models, budget caps and worktree finalization. One disclosure
// keeps the first-launch page to target + inputs.

import type { VarField } from "@/api/types";
import { Badge } from "@/components/ui/Badge";
import { useBackendDetectStore } from "@/store/backendDetect";

import AutoManagedVarsSection from "./AutoManagedVarsSection";
import BudgetSection from "./BudgetSection";
import ModelOverridesSection, { type LLMNode } from "./ModelOverridesSection";
import RunSettingsSection from "./RunSettingsSection";
import type { UseRunOverridesResult } from "./useRunOverrides";
import VarFieldsSection from "./VarFieldsSection";
import WorktreeFinalizationSection from "./WorktreeFinalizationSection";

export interface AdvancedSectionProps {
  overrides: UseRunOverridesResult;
  fields: VarField[];
  advancedVarFields: VarField[];
  autoManagedFields: VarField[];
  values: Record<string, string>;
  submitting: boolean;
  onValueChange: (name: string, value: string) => void;
  onSubmit: () => void;
  llmNodes: LLMNode[];
  worktreeOn: boolean;
  cloud: boolean;
  provider: string | null;
}

export default function AdvancedSection({
  overrides,
  fields,
  advancedVarFields,
  autoManagedFields,
  values,
  submitting,
  onValueChange,
  onSubmit,
  llmNodes,
  worktreeOn,
  cloud,
  provider,
}: AdvancedSectionProps) {
  const backendReport = useBackendDetectStore((s) => s.report);

  // Option count surfaced on the Advanced toggle so the collapsed state
  // still says how much is tucked away: optional + auto-managed inputs,
  // the three run-settings knobs (+ review topology when declared),
  // per-node model overrides, five budget caps, four worktree fields.
  const advancedCount =
    advancedVarFields.length +
    autoManagedFields.length +
    3 +
    (fields.some((f) => f.name === "review_mode") ? 1 : 0) +
    llmNodes.length +
    5 +
    4;

  return (
    <div className="mt-4 border-t border-border-subtle pt-2">
      <button
        type="button"
        onClick={overrides.toggleAdvanced}
        aria-expanded={overrides.advancedOpen}
        className="flex w-full items-center gap-1.5 py-1 text-xs font-medium text-fg-muted hover:text-fg-default"
      >
        <span aria-hidden>{overrides.advancedOpen ? "▾" : "▸"}</span>
        Advanced
        <Badge variant="neutral" size="sm">
          {advancedCount} option{advancedCount === 1 ? "" : "s"}
        </Badge>
        <span className="font-normal text-fg-subtle">
          inputs · backend · models · budget · worktree
        </span>
      </button>
      {overrides.advancedOpen && (
        <>
          <div className="mt-3">
            <VarFieldsSection
              fields={advancedVarFields}
              title="Optional inputs"
              values={values}
              submitting={submitting}
              onValueChange={onValueChange}
              onSubmit={onSubmit}
            />
            <AutoManagedVarsSection
              fields={autoManagedFields}
              values={values}
              submitting={submitting}
              onValueChange={onValueChange}
            />
          </div>
          <RunSettingsSection
            backendOverride={overrides.backendOverride}
            compressOverride={overrides.compressOverride}
            permissionOverride={overrides.permissionOverride}
            reviewModeOverride={overrides.reviewModeOverride}
            backendReport={backendReport}
            effective={overrides.effectiveSettings}
            onBackendChange={overrides.setBackendOverride}
            onCompressChange={overrides.setCompressOverride}
            onPermissionChange={overrides.setPermissionOverride}
            onReviewModeChange={overrides.setReviewModeOverride}
            showReviewMode={fields.some((f) => f.name === "review_mode")}
          />
          <ModelOverridesSection
            nodes={llmNodes}
            overrides={overrides.modelOverrides}
            backendReport={backendReport}
            onChange={(name, patch) =>
              overrides.setModelOverrides((prev) => ({
                ...prev,
                [name]: { ...prev[name], ...patch },
              }))
            }
          />
          <BudgetSection
            show={overrides.showBudget}
            values={overrides.budgetFields}
            onToggle={() => overrides.setShowBudget((v) => !v)}
            onChange={(patch) =>
              overrides.setBudgetFields((prev) => ({ ...prev, ...patch }))
            }
          />
          <WorktreeFinalizationSection
            showAdvanced={overrides.showAdvanced}
            worktreeOn={worktreeOn}
            mergeInto={overrides.mergeInto}
            branchName={overrides.branchName}
            mergeStrategy={overrides.mergeStrategy}
            autoMerge={overrides.autoMerge}
            onToggle={() => overrides.setShowAdvanced((v) => !v)}
            onMergeIntoChange={overrides.setMergeInto}
            onBranchNameChange={overrides.setBranchName}
            onMergeStrategyChange={overrides.setMergeStrategy}
            onAutoMergeChange={overrides.setAutoMerge}
            cloud={cloud}
            provider={provider}
          />
        </>
      )}
    </div>
  );
}
