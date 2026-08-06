// Extracted from LaunchView.tsx to keep that file focused.
// useRunOverrides owns every per-run tuning knob the Advanced disclosure
// renders: backend/compress/permission/review-mode overrides, per-node
// model overrides, budget caps, worktree finalization fields, and the
// disclosure open/closed state itself.

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { previewRunCost } from "@/api/runs";
import type { MergeStrategy } from "@/api/runs";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";

import {
  emptyBudgetFieldValues,
  type BudgetFieldValues,
} from "./budgetPayload";
import { type NodeOverride } from "./ModelOverridesSection";

const BOT_OPTIONS_OPEN_KEY = "iterion.launch.bot-options-open";
const ENGINE_OPTIONS_OPEN_KEY = "iterion.launch.engine-options-open";

export type UseRunOverridesResult = ReturnType<typeof useRunOverrides>;

export function useRunOverrides(filePath: string, worktreeOn: boolean) {
  // Worktree finalization overrides — only meaningful when the
  // workflow declares `worktree: auto`. We always render the controls
  // (collapsed) even for non-worktree runs so the UI is predictable;
  // the backend ignores them when worktree is off.
  const [mergeInto, setMergeInto] = useState<string>(""); // "" = current
  const [branchName, setBranchName] = useState<string>("");
  const [mergeStrategy, setMergeStrategy] = useState<MergeStrategy>("squash");
  // GitLab-style "auto-merge when run finishes". Default off so the
  // run lands as a "pending merge" and the user picks the strategy
  // after seeing the commits — GitHub-PR style.
  const [autoMerge, setAutoMerge] = useState<boolean>(false);
  // showAdvanced opens the worktree finalization block. Default off,
  // but auto-opens once the loaded workflow is detected to use
  // `worktree: auto` so users see the squash/merge + auto-merge
  // controls without having to click — they're meaningful options,
  // not "advanced" in the obscure sense.
  const [showAdvanced, setShowAdvanced] = useState(false);
  // Two sibling disclosures below the primary inputs — "Bot options"
  // (the bot's optional/auto-managed inputs) and "Engine options"
  // (backend/model/budget/worktree tuning, identical for every bot) —
  // both collapsed on first launch so the page reads target + inputs.
  // Open/closed state persists per disclosure across visits (power users
  // keep them open, everyone else keeps the short form).
  const [botOptionsOpen, setBotOptionsOpen] = useState(() =>
    readBooleanFlag(BOT_OPTIONS_OPEN_KEY),
  );
  const toggleBotOptions = () => {
    const next = !botOptionsOpen;
    setBotOptionsOpen(next);
    writeBooleanFlag(BOT_OPTIONS_OPEN_KEY, next);
  };
  const [engineOptionsOpen, setEngineOptionsOpen] = useState(() =>
    readBooleanFlag(ENGINE_OPTIONS_OPEN_KEY),
  );
  const toggleEngineOptions = () => {
    const next = !engineOptionsOpen;
    setEngineOptionsOpen(next);
    writeBooleanFlag(ENGINE_OPTIONS_OPEN_KEY, next);
  };
  // Backend override for this run. "" = let the resolver pick (the
  // current behaviour, mirrored in Settings → Backends).
  // Sending an explicit name overrides the workflow's `default_backend:`
  // but node-level explicit `backend:` still wins.
  const [backendOverride, setBackendOverride] = useState<string>("");
  // command-output-compression override for this run ("" inherits the
  // workflow/node `compress:` DSL then ITERION_COMPRESS).
  const [compressOverride, setCompressOverride] = useState<string>("");
  // auto-memory (MEMORY.md) override for this run ("" inherits the
  // workflow/node `auto_memory:` DSL then ITERION_AUTO_MEMORY; default off).
  const [autoMemoryOverride, setAutoMemoryOverride] = useState<string>("");
  // tool-permission gate mode override ("" inherits the workflow/node
  // `permission:` DSL then ITERION_PERMISSION).
  const [permissionOverride, setPermissionOverride] = useState<string>("");
  // Server-resolved knob provenance below run-override (workflow/env/
  // default), captioning the Run-settings selects. Best-effort — any
  // failure just leaves the captions hidden.
  const effectiveQuery = useQuery({
    queryKey: ["preview-effective-settings", filePath],
    queryFn: async () =>
      (await previewRunCost({ file_path: filePath })).effective ?? null,
    enabled: !!filePath,
  });
  const effectiveSettings = effectiveQuery.isError
    ? null
    : effectiveQuery.data ?? null;
  // Mono/dual review-topology override ("" = auto: resolve from the
  // providers detected at launch). Only sent when explicitly mono/dual;
  // ignored by bots that don't declare a `review_mode` var.
  const [reviewModeOverride, setReviewModeOverride] = useState<string>("");
  // Per-run budget-cap overrides (cost / tokens / duration / iterations /
  // parallel branches). Raw input strings; empty = inherit the bot's
  // `budget:` block. Folded into createRun.budget via buildBudgetPayload.
  const [budgetFields, setBudgetFields] = useState<BudgetFieldValues>(
    emptyBudgetFieldValues,
  );
  // Opens the collapsible budget-overrides block. Default collapsed —
  // most runs inherit the bot's budget untouched.
  const [showBudget, setShowBudget] = useState(false);
  // Per-node model/backend overrides, keyed by node name. Empty fields =
  // inherit the bot's DSL default. Folded into createRun.model_overrides.
  const [modelOverrides, setModelOverrides] = useState<Record<string, NodeOverride>>({});
  // Run-level fallback route (ADR-087): one alternative applied to agent
  // nodes that declare no `fallbacks:` of their own, never to judges.
  // Empty backend = off, which is the default.
  const [fallbackBackend, setFallbackBackend] = useState<string>("");
  const [fallbackModel, setFallbackModel] = useState<string>("");

  // Auto-open the worktree-finalization block once the document loads
  // and the workflow uses worktree:auto. Done in an effect so it only
  // fires after `doc` is populated and doesn't fight a user who
  // explicitly closed the section afterwards (we only flip false→true).
  useEffect(() => {
    if (worktreeOn) setShowAdvanced(true);
  }, [worktreeOn]);

  return {
    fallbackBackend,
    setFallbackBackend,
    fallbackModel,
    setFallbackModel,
    mergeInto,
    setMergeInto,
    branchName,
    setBranchName,
    mergeStrategy,
    setMergeStrategy,
    autoMerge,
    setAutoMerge,
    showAdvanced,
    setShowAdvanced,
    botOptionsOpen,
    toggleBotOptions,
    engineOptionsOpen,
    toggleEngineOptions,
    backendOverride,
    setBackendOverride,
    compressOverride,
    autoMemoryOverride,
    setAutoMemoryOverride,
    setCompressOverride,
    permissionOverride,
    setPermissionOverride,
    effectiveSettings,
    reviewModeOverride,
    setReviewModeOverride,
    budgetFields,
    setBudgetFields,
    showBudget,
    setShowBudget,
    modelOverrides,
    setModelOverrides,
  };
}
