// Extracted from LaunchView.tsx to keep that file focused.
// useLaunchSubmit owns the launch flow: the missing-required-field gate
// (attachments before vars, then repo target), the no-sandbox
// confirmation, and the createRun submission itself (including the
// stage-a-forge-repo step for "create" repo targets).

import { useEffect, useState } from "react";
import { useLocation } from "wouter";

import { createForgeRepo } from "@/api/forgeConnections";
import { createRun } from "@/api/runs";
import type { AttachmentField, IterDocument, VarField } from "@/api/types";
import { errorMessage } from "@/lib/errorHints";
import { isVarMissing } from "@/lib/varValidation";

import { type AttachmentValue } from "../AttachmentFieldInput";
import { buildBudgetPayload } from "./budgetPayload";
import { findAttachedRepo } from "./RepoTargetSection";
import type { useBotPresets } from "./useBotPresets";
import type { UseRepoTargetResult } from "./useRepoTarget";
import type { UseRunOverridesResult } from "./useRunOverrides";
import { isSandboxActive, literalToString } from "./utils";
import { isAutoManagedVar } from "./varClassify";
import { buildVarsPayload } from "./varsPayload";

export interface UseLaunchSubmitArgs {
  filePath: string;
  doc: IterDocument | null;
  currentSource: string | null;
  fields: VarField[];
  values: Record<string, string>;
  attachmentFields: AttachmentField[];
  attachments: Record<string, AttachmentValue | null>;
  selectedPreset: string;
  selectedPresetMeta: ReturnType<typeof useBotPresets>["selectedPresetMeta"];
  repoTarget: UseRepoTargetResult;
  overrides: UseRunOverridesResult;
  setError: (msg: string | null) => void;
}

export function useLaunchSubmit({
  filePath,
  doc,
  currentSource,
  fields,
  values,
  attachmentFields,
  attachments,
  selectedPreset,
  selectedPresetMeta,
  repoTarget,
  overrides,
  setError,
}: UseLaunchSubmitArgs) {
  const [, setLocation] = useLocation();
  const [submitting, setSubmitting] = useState(false);
  // Set when the user clicks Launch on a workflow with no sandbox
  // active. Surfaces the ConfirmDialog so they make a deliberate
  // choice (host execution carries real risk: any tool the bot calls
  // runs against the operator's machine).
  const [showNoSandboxConfirm, setShowNoSandboxConfirm] = useState(false);

  const {
    activeRepo,
    teamRepos,
    teamID,
    repoRequirement,
    showRepoTarget,
    repoTargetState,
    setRepoTargetCreateError,
    missingRepoTarget,
  } = repoTarget;

  // First unfilled required field, in precedence order (attachments before
  // vars, then repo target). One walk feeds the launch gate, the caption
  // text, and the scroll/focus target — so the precedence can't drift
  // across them.
  const missingAttachmentField = attachmentFields.find(
    (f) => f.required && !attachments[f.name]?.uploadId,
  );
  // Auto-managed vars count as satisfied: the runner resolves their
  // placeholder default at start, so a required-but-auto var never blocks.
  const missingVarField = fields.find(
    (f) => !isAutoManagedVar(f) && isVarMissing(f, values[f.name] ?? ""),
  );
  const missingRequired = !!(missingAttachmentField || missingVarField || missingRepoTarget);
  const missingTitle = missingAttachmentField
    ? `Required attachment missing: ${missingAttachmentField.name}`
    : missingVarField
      ? `Required input missing: ${missingVarField.name}`
      : missingRepoTarget
        ? "Target repository required"
        : undefined;
  const firstMissingFieldId = missingAttachmentField
    ? `attach-${missingAttachmentField.name}`
    : missingVarField
      ? `var-${missingVarField.name}`
      : missingRepoTarget
        ? "repo-target-section"
        : null;

  // Tracks whether Launch was pressed while required fields are still
  // missing — promotes the inline caption from polite to assertive and
  // (via onSubmit) scrolls/focuses the first gap instead of leaving the
  // user staring at a silently disabled button. Reset once the form is
  // complete so the next blocked attempt re-announces.
  const [attemptedLaunch, setAttemptedLaunch] = useState(false);
  useEffect(() => {
    if (!missingRequired) setAttemptedLaunch(false);
  }, [missingRequired]);

  // launchRun runs the actual createRun call. Separated from the
  // user-facing onSubmit so the no-sandbox ConfirmDialog can reach it
  // directly when the user accepts the warning.
  const launchRun = async () => {
    setSubmitting(true);
    setError(null);
    setRepoTargetCreateError(null);
    // Resolve the repo target (attach / create / active / none). For
    // "create" we first mint the forge repo, then feed its clone_url +
    // connection_id into createRun below. Errors here are inline in the
    // section (createError), not the page-level banner — they're
    // actionable in-place (409 name taken, 422 App perms).
    let repoUrl: string | undefined;
    let connectionID: string | undefined;
    if (showRepoTarget && repoTargetState && repoRequirement && teamID) {
      switch (repoTargetState.mode) {
        case "active":
          if (activeRepo?.clone_url) {
            repoUrl = activeRepo.clone_url;
            connectionID = activeRepo.connection_id;
          }
          break;
        case "attach": {
          const chosen = findAttachedRepo(teamRepos, repoTargetState.attachKey);
          if (chosen?.clone_url) {
            repoUrl = chosen.clone_url;
            connectionID = chosen.connection_id;
          }
          break;
        }
        case "create":
          try {
            const created = await createForgeRepo(teamID, {
              connection_id: repoTargetState.createConnectionID,
              owner: repoTargetState.createOwner || undefined,
              name: repoTargetState.createName.trim(),
              private: repoTargetState.createPrivate,
              default_branch: repoRequirement.default_branch || undefined,
              init_readme: true,
            });
            repoUrl = created.clone_url;
            connectionID = repoTargetState.createConnectionID;
          } catch (e) {
            setRepoTargetCreateError(errorMessage(e));
            setSubmitting(false);
            return;
          }
          break;
        case "none":
          // Explicit skip — send nothing new.
          break;
      }
    }
    try {
      const attachmentsPayload: Record<string, string> = {};
      for (const f of attachmentFields) {
        const a = attachments[f.name];
        if (a?.uploadId) attachmentsPayload[f.name] = a.uploadId;
      }
      const res = await createRun({
        file_path: filePath,
        source: currentSource || undefined,
        // Only touched values are sent: a var left at its declared
        // default (auto-managed or not) is omitted so the server applies
        // its own default + ${...} placeholder expansion. Keys the active
        // preset covers compare against the preset's value instead — the
        // server applies preset values before spec.Vars, so a field edited
        // back to its declared default must still be sent to win.
        vars: buildVarsPayload(
          fields,
          values,
          selectedPresetMeta
            ? Object.fromEntries(
                selectedPresetMeta.values.map((pv) => [
                  pv.key,
                  literalToString(pv.value),
                ]),
              )
            : undefined,
        ),
        preset: selectedPreset || undefined,
        merge_into: overrides.mergeInto || undefined,
        branch_name: overrides.branchName || undefined,
        merge_strategy: overrides.mergeStrategy,
        auto_merge: overrides.autoMerge,
        attachments:
          Object.keys(attachmentsPayload).length > 0 ? attachmentsPayload : undefined,
        backend: overrides.backendOverride || undefined,
        compress: overrides.compressOverride || undefined,
      auto_memory: overrides.autoMemoryOverride || undefined,
        permission: overrides.permissionOverride || undefined,
        review_mode:
          overrides.reviewModeOverride && overrides.reviewModeOverride !== "auto"
            ? overrides.reviewModeOverride
            : undefined,
        budget: buildBudgetPayload(overrides.budgetFields),
        model_overrides: (() => {
          const entries = Object.entries(overrides.modelOverrides)
            .map(([selector, o]) => ({
              selector,
              model: o.model?.trim() || undefined,
              backend: o.backend || undefined,
            }))
            .filter((e) => e.model || e.backend);
          return entries.length > 0 ? entries : undefined;
        })(),
        fallback: overrides.fallbackBackend
          ? {
              backend: overrides.fallbackBackend,
              model: overrides.fallbackModel.trim() || undefined,
            }
          : undefined,
        repo_url: repoUrl,
        connection_id: connectionID,
      });
      setLocation(`/runs/${encodeURIComponent(res.run_id)}`);
    } catch (e) {
      setError(errorMessage(e));
      setSubmitting(false);
    }
  };

  // onSubmit is the click target. Intercepts the launch when the
  // workflow has no sandbox declared and opens the ConfirmDialog;
  // otherwise calls launchRun directly.
  const onSubmit = () => {
    // Soft-block: rather than a silently disabled button, a blocked Launch
    // scrolls to and focuses the first missing required field.
    if (missingRequired) {
      setAttemptedLaunch(true);
      if (firstMissingFieldId) {
        const targetId = firstMissingFieldId;
        requestAnimationFrame(() => {
          const el = document.getElementById(targetId);
          if (!el) return;
          el.scrollIntoView({ behavior: "smooth", block: "center" });
          const focusable = el.matches("input, textarea, select, button")
            ? el
            : el.querySelector<HTMLElement>(
                "input, textarea, select, button, [tabindex]:not([tabindex='-1'])",
              );
          focusable?.focus({ preventScroll: true });
        });
      }
      return;
    }
    if (!isSandboxActive(doc)) {
      setShowNoSandboxConfirm(true);
      return;
    }
    void launchRun();
  };

  return {
    submitting,
    showNoSandboxConfirm,
    setShowNoSandboxConfirm,
    attemptedLaunch,
    missingRequired,
    missingTitle,
    launchRun,
    onSubmit,
  };
}
