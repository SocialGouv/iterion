// "Engine options" — the iterion engine's per-run tuning, identical in
// content and order for every bot: backend/compress/permission overrides,
// per-node model overrides, budget caps, worktree finalization. Sibling of
// BotOptionsSection (the bot's own optional inputs); always rendered so
// operators find the engine knobs in the same place on every launch page.

import type { VarField } from "@/api/types";
import { useBackendDetectStore } from "@/store/backendDetect";

import BudgetSection from "./BudgetSection";
import FallbackSection from "./FallbackSection";
import ModelOverridesSection, { type LLMNode } from "./ModelOverridesSection";
import OptionsDisclosure from "./OptionsDisclosure";
import RunSettingsSection from "./RunSettingsSection";
import type { UseRunOverridesResult } from "./useRunOverrides";
import WorktreeFinalizationSection from "./WorktreeFinalizationSection";

export interface EngineOptionsSectionProps {
  overrides: UseRunOverridesResult;
  /** Full declared var list — only read to detect a `review_mode` var
   *  (which adds the review-topology knob to Run settings). */
  fields: VarField[];
  llmNodes: LLMNode[];
  worktreeOn: boolean;
  cloud: boolean;
  provider: string | null;
}

export default function EngineOptionsSection({
  overrides,
  fields,
  llmNodes,
  worktreeOn,
  cloud,
  provider,
}: EngineOptionsSectionProps) {
  const backendReport = useBackendDetectStore((s) => s.report);

  const showReviewMode = fields.some((f) => f.name === "review_mode");
  // Option count surfaced on the collapsed toggle: the three run-settings
  // knobs (+ review topology when declared), per-node model overrides,
  // five budget caps, four worktree fields.
  // …plus the one run-level fallback row.
  const count = 3 + (showReviewMode ? 1 : 0) + llmNodes.length + 5 + 4 + 1;

  return (
    <OptionsDisclosure
      label="Engine options"
      count={count}
      hint="backend · models · budget · worktree"
      open={overrides.engineOptionsOpen}
      onToggle={overrides.toggleEngineOptions}
    >
      <RunSettingsSection
        backendOverride={overrides.backendOverride}
        compressOverride={overrides.compressOverride}
        autoMemoryOverride={overrides.autoMemoryOverride}
        permissionOverride={overrides.permissionOverride}
        reviewModeOverride={overrides.reviewModeOverride}
        backendReport={backendReport}
        effective={overrides.effectiveSettings}
        onBackendChange={overrides.setBackendOverride}
        onCompressChange={overrides.setCompressOverride}
        onAutoMemoryChange={overrides.setAutoMemoryOverride}
        onPermissionChange={overrides.setPermissionOverride}
        onReviewModeChange={overrides.setReviewModeOverride}
        showReviewMode={showReviewMode}
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
      <FallbackSection
        backend={overrides.fallbackBackend}
        model={overrides.fallbackModel}
        backendReport={backendReport}
        cloud={cloud}
        onBackendChange={overrides.setFallbackBackend}
        onModelChange={overrides.setFallbackModel}
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
    </OptionsDisclosure>
  );
}
