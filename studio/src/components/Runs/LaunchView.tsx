import { useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import BotIdentity from "@/components/shared/BotIdentity";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { DesktopOnlyNotice } from "@/components/ui/DesktopOnlyNotice";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useServerInfoStore } from "@/store/serverInfo";

import AdvancedSection from "./launchView/AdvancedSection";
import AttachmentsSection from "./launchView/AttachmentsSection";
import LaunchBar from "./launchView/LaunchBar";
import NoSandboxConfirmDialog from "./launchView/NoSandboxConfirmDialog";
import NoSourceEmptyState from "./launchView/NoSourceEmptyState";
import PresetSection from "./launchView/PresetSection";
import RepoTargetSection from "./launchView/RepoTargetSection";
import { useAttachmentUploads } from "./launchView/useAttachmentUploads";
import { useBotPresets } from "./launchView/useBotPresets";
import { useLaunchDoc } from "./launchView/useLaunchDoc";
import { useLaunchServerInfo } from "./launchView/useLaunchServerInfo";
import { useLaunchSubmit } from "./launchView/useLaunchSubmit";
import { useRepoTarget } from "./launchView/useRepoTarget";
import { useRunOverrides } from "./launchView/useRunOverrides";
import { sandboxModeLabel } from "./launchView/utils";
import VarFieldsSection from "./launchView/VarFieldsSection";

// LaunchView orchestrates the launch form. All state lives in the
// launchView/ hooks (document + values, presets, attachments, repo
// target, run overrides, submission flow); this component wires them to
// the section components.
export default function LaunchView() {
  const [, setLocation] = useLocation();
  const search = useSearch();
  const filePath = useMemo(() => {
    const params = new URLSearchParams(search);
    return params.get("file") ?? "";
  }, [search]);

  // Page-level error banner — fed by both document loading and the
  // launch submission (which clears it before each attempt).
  const [error, setError] = useState<string | null>(null);

  const serverInfo = useLaunchServerInfo();
  const {
    doc,
    noSource,
    currentSource,
    values,
    setValues,
    fields,
    primaryFields,
    advancedVarFields,
    autoManagedFields,
    attachmentFields,
    llmNodes,
    worktreeOn,
  } = useLaunchDoc(filePath, setError);
  const { bot, presets, selectedPreset, selectedPresetMeta, applyPreset } =
    useBotPresets(filePath, doc, setValues);
  const { attachments, handleAttachmentChange } = useAttachmentUploads();
  const repoTarget = useRepoTarget(bot, serverInfo);
  const overrides = useRunOverrides(filePath, worktreeOn);
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");

  const {
    submitting,
    showNoSandboxConfirm,
    setShowNoSandboxConfirm,
    attemptedLaunch,
    missingRequired,
    missingTitle,
    launchRun,
    onSubmit,
  } = useLaunchSubmit({
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
  });

  const limits = serverInfo?.limits.upload ?? null;
  const onValueChange = (name: string, value: string) =>
    setValues((prev) => ({ ...prev, [name]: value }));

  useHeaderSlot({
    left: (
      <>
        <span className="text-xs font-semibold text-fg-muted">
          {bot ? `Launch — ${bot.display_name?.trim() || bot.name}` : "Launch run"}
        </span>
        {/* "Unsaved workflow" only when there IS a workflow (in-memory
            doc without a file). With nothing to launch at all, a
            subtitle would contradict the empty state below. */}
        {(filePath || doc) && (
          <span
            className="text-xs text-fg-subtle font-mono truncate max-w-md"
            title={filePath || "Unsaved workflow"}
          >
            {filePath || "Unsaved workflow"}
          </span>
        )}
      </>
    ),
    right: (
      <Button
        variant="ghost"
        size="sm"
        onClick={() => setLocation("/editor")}
      >
        Cancel
      </Button>
    ),
  });

  if (noSource) {
    return <NoSourceEmptyState cloud={cloud} onNavigate={setLocation} />;
  }

  return (
    <div className="h-full flex flex-col bg-surface-1 text-fg-default">
      <div className="flex-1 overflow-auto px-4 py-4 max-w-3xl">
        <DesktopOnlyNotice
          feature="the Launch form"
          hint="Variable inputs, attachment uploads, and worktree-finalization toggles are designed for desktop interaction. View runs on phones; launch from desktop."
          lsKey="iterion.launch.mobile-optin"
        >
        {error && (
          <InlineBanner tone="danger" layout="inline" className="mb-3">
            {error}
          </InlineBanner>
        )}
        {!doc && !error ? (
          <div className="text-xs text-fg-subtle">Loading workflow…</div>
        ) : (
          <>
            {/* Persona header: who runs, in the bot's own identity. The
                workflow file path demotes to a small secondary line. */}
            {bot && (
              <div className="mb-6">
                <BotIdentity
                  bot={bot}
                  size="md"
                  meta={
                    filePath ? (
                      <p
                        className="mt-1 font-mono text-caption text-fg-subtle truncate"
                        title={filePath}
                      >
                        {filePath}
                      </p>
                    ) : undefined
                  }
                />
              </div>
            )}
            {/* The target repository is the launch's primary decision for
                repo-declaring bots — it leads, mirroring the wizard's
                one-decision-first ordering. */}
            {repoTarget.showRepoTarget &&
              repoTarget.repoRequirement &&
              repoTarget.repoTargetState && (
                <div id="repo-target-section">
                  <RepoTargetSection
                    repo={repoTarget.repoRequirement}
                    activeRepo={repoTarget.activeRepo}
                    repos={repoTarget.teamRepos}
                    teamID={repoTarget.teamID}
                    state={repoTarget.repoTargetState}
                    onChange={(patch) =>
                      repoTarget.setRepoTargetState((prev) =>
                        prev ? { ...prev, ...patch } : prev,
                      )
                    }
                    createError={repoTarget.repoTargetCreateError}
                    submitting={submitting}
                    filePath={filePath}
                  />
                </div>
              )}
            <AttachmentsSection
              fields={attachmentFields}
              attachments={attachments}
              limits={limits}
              submitting={submitting}
              onChange={(f, next) => void handleAttachmentChange(f, next)}
            />
            <PresetSection
              presets={presets}
              selectedPreset={selectedPreset}
              selectedPresetMeta={selectedPresetMeta}
              filePath={filePath}
              submitting={submitting}
              onApply={applyPreset}
              onEditInEditor={() =>
                setLocation(
                  `/editor?file=${encodeURIComponent(filePath)}&focus=presets`,
                )
              }
            />
            <VarFieldsSection
              fields={primaryFields}
              values={values}
              submitting={submitting}
              onValueChange={onValueChange}
              onSubmit={onSubmit}
              emptyFallback={
                fields.length === 0 ? (
                  attachmentFields.length === 0 ? (
                    <div className="space-y-1">
                      <p className="text-xs text-fg-subtle">
                        This workflow declares no input vars. You can launch it as-is.
                      </p>
                      <p className="text-caption text-fg-subtle">
                        The workflow&apos;s prompts will read directly from{" "}
                        <code>vars:</code> defaults.
                      </p>
                    </div>
                  ) : null
                ) : (
                  <p className="text-xs text-fg-subtle">
                    No required inputs — every var has a default. Tune them
                    under Advanced, or launch as-is.
                  </p>
                )
              }
            />
            {/* Everything below is tuning, not launch-blocking: one
                disclosure keeps the first-launch page to target + inputs
                (the wizard bar), while power users open it for optional/
                auto-managed inputs, backend, per-node models, budget caps
                and worktree finalization. */}
            <AdvancedSection
              overrides={overrides}
              fields={fields}
              advancedVarFields={advancedVarFields}
              autoManagedFields={autoManagedFields}
              values={values}
              submitting={submitting}
              onValueChange={onValueChange}
              onSubmit={onSubmit}
              llmNodes={llmNodes}
              worktreeOn={worktreeOn}
              cloud={cloud}
              provider={repoTarget.selectedRepoProvider}
            />

            <LaunchBar
              docReady={!!doc}
              submitting={submitting}
              missingRequired={missingRequired}
              missingTitle={missingTitle}
              attemptedLaunch={attemptedLaunch}
              sandboxMode={sandboxModeLabel(doc)}
              filePath={filePath}
              currentSource={currentSource}
              onSubmit={onSubmit}
            />
          </>
        )}
        </DesktopOnlyNotice>
      </div>

      <NoSandboxConfirmDialog
        open={showNoSandboxConfirm}
        cloud={cloud}
        onConfirm={() => {
          setShowNoSandboxConfirm(false);
          void launchRun();
        }}
        onCancel={() => setShowNoSandboxConfirm(false)}
        onEditWorkflow={() => {
          setShowNoSandboxConfirm(false);
          setLocation("/editor");
        }}
      />
    </div>
  );
}
