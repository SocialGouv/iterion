import { useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import { useMaybeAuth } from "@/auth/AuthContext";
import { useBotsStore } from "@/store/bots";
import { useTabsStore } from "@/store/tabs";
import BotIdentity from "@/components/shared/BotIdentity";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { DesktopOnlyNotice } from "@/components/ui/DesktopOnlyNotice";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useServerInfoStore } from "@/store/serverInfo";

import AttachmentsSection from "./launchView/AttachmentsSection";
import BotOptionsSection from "./launchView/BotOptionsSection";
import { resolveBotSlug } from "./launchView/botSlugTarget";
import EngineOptionsSection from "./launchView/EngineOptionsSection";
import LaunchBar from "./launchView/LaunchBar";
import { applyLaunchHints } from "./launchView/launchHints";
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
// One form per TARGET. Everything the form holds is about one launch — the
// values typed, the preset, the engine options folded out of sight, the
// attachments, the banner — so the form is keyed by what it launches, the
// team included, and a move to another target starts from nothing rather
// than carry any of it where the operator cannot see it. Under a file or a
// bot the editor is not part of the target (an editor event is no reason to
// reset the form); the unsaved buffer's form is its editor tab's.
export default function LaunchView() {
  const search = useSearch();
  const teamScope = useMaybeAuth()?.activeTeamID ?? "";
  const bufferTab = useTabsStore((s) => s.activeEditorTabId) ?? "";
  const params = new URLSearchParams(search);
  const file = params.get("file") ?? "";
  const bot = file ? "" : (params.get("bot") ?? "");
  const target =
    file || bot ? `${teamScope}\u0000${file}\u0000${bot}` : `${teamScope}\u0000buffer\u0000${bufferTab}`;
  return <LaunchForm key={target} />;
}

function LaunchForm() {
  const [, setLocation] = useLocation();
  const search = useSearch();
  const allBots = useBotsStore((s) => s.bots);
  const botsError = useBotsStore((s) => s.error);
  const { fileParam, botParam } = useMemo(() => {
    const params = new URLSearchParams(search);
    return {
      fileParam: params.get("file") ?? "",
      botParam: params.get("bot") ?? "",
    };
  }, [search]);
  const { serverInfo, serverInfoPending } = useLaunchServerInfo();
  // `?bot=<slug>` is the clean, shareable launch URL (e.g.
  // /runs/new?bot=feed-watch). `?file=` still wins when both are present
  // (explicit path beats slug). What the slug names is resolveBotSlug's
  // decision: the bot by name in cloud — the form renders the program the
  // server resolves and the launch posts the bot's id, pinned to it — its
  // workflow file locally, and otherwise a wait or a refusal by name. Under a
  // bot's URL the form never renders the editor's buffer in its place.
  // The catalog a local slug resolves against is the one useBotPresets asks
  // for whenever it is not there.
  const botSlug = fileParam ? "" : botParam;
  const slugTarget = useMemo(
    () =>
      resolveBotSlug({
        slug: botSlug,
        serverInfo,
        serverInfoPending,
        bots: allBots,
        botsError,
      }),
    [botSlug, serverInfo, serverInfoPending, allBots, botsError],
  );
  const namedSlug = slugTarget.state === "bot" ? slugTarget.bot : "";
  const filePath = fileParam || (slugTarget.state === "file" ? slugTarget.file : "");
  // Waiting or refused, the form holds: it loads nothing and arms no submit.
  const slugHeld = slugTarget.state === "pending" || slugTarget.state === "refused";

  // Page-level error banner — fed by both document loading and the
  // launch submission (which clears it before each attempt).
  const [loadError, setError] = useState<string | null>(null);
  // Set when the server refused the launch because the bot changed under the
  // form: the banner then offers to read the bot's program again.
  const [programChanged, setProgramChanged] = useState(false);
  // Where to look before launching again, after a launch that drew no answer
  // about its run.
  const [unansweredRuns, setUnansweredRuns] = useState<string | null>(null);
  // An error is about ONE launch: when the URL moves to another program, the
  // banner of the one before — and the reload it offered — goes with it.
  // Discarded during the render that sees the move (React's
  // adjust-state-when-a-prop-changes shape), so the load of the new target
  // reports into a clean slate.
  const targetKey = `${fileParam}\u0000${botSlug}`;
  const [errorTarget, setErrorTarget] = useState(targetKey);
  if (errorTarget !== targetKey) {
    setErrorTarget(targetKey);
    setError(null);
    setProgramChanged(false);
    setUnansweredRuns(null);
  }
  const error = slugTarget.state === "refused" ? slugTarget.reason : loadError;

  const {
    doc,
    noSource,
    currentSource,
    namedBot,
    namedPresets,
    reload,
    confirmedDiskPath,
    values,
    setValues,
    fields,
    attachmentFields,
    llmNodes,
    worktreeOn,
    load,
  } = useLaunchDoc(filePath, setError, { bot: namedSlug, pending: slugHeld });
  const { bot, presets, selectedPreset, selectedPresetMeta, applyPreset } =
    useBotPresets(
      filePath,
      doc,
      setValues,
      // A named launch's presets are the ones the server read with its
      // program; the registry entry supplies the bot's identity, its launch
      // hints and its repository requirement — nothing a launch runs.
      namedBot && namedPresets ? { botID: namedBot.id, presets: namedPresets } : null,
      load,
    );
  // Progressive-disclosure buckets, folding in the resolved bot's launch
  // hints (manifest `launch:` block): hint-forced primaries lead, hidden
  // vars never render, everything else falls back to the varClassify
  // heuristics. Presentation-only — validation + payload read `fields`.
  const {
    primary: primaryFields,
    advanced: advancedVarFields,
    auto: autoManagedFields,
    hintedPrimary,
  } = applyLaunchHints(fields, bot?.launch);
  const { attachments, handleAttachmentChange } = useAttachmentUploads();
  const repoTarget = useRepoTarget(bot, serverInfo);
  const overrides = useRunOverrides(filePath, currentSource, worktreeOn);
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
    // A launch by name posts the bot's id and the digest its read issued —
    // `null` until that read lands, which carries no program.
    namedBot: namedSlug ? namedBot : undefined,
    onProgramChanged: setProgramChanged,
    onUnanswered: setUnansweredRuns,
    confirmedDiskPath,
    serverInfo,
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
        {(filePath || namedSlug || doc) && (
          <span
            className="text-xs text-fg-subtle font-mono truncate max-w-md"
            title={filePath || namedSlug || "Unsaved workflow"}
          >
            {filePath || namedSlug || "Unsaved workflow"}
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
          <InlineBanner
            tone="danger"
            layout="inline"
            className="mb-3"
            action={
              // The remedy of a launch refused because the bot changed: read
              // the bot's program again, on the operator's own gesture — the
              // fields they filled are about to be replaced.
              programChanged && namedSlug ? (
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => {
                    setError(null);
                    setProgramChanged(false);
                    reload();
                  }}
                >
                  Reload the bot
                </Button>
              ) : unansweredRuns ? (
                // No answer said whether the run exists: the way to look,
                // before a second click starts a second one.
                <Button variant="secondary" size="sm" onClick={() => setLocation(unansweredRuns)}>
                  Open its runs
                </Button>
              ) : undefined
            }
          >
            {error}
          </InlineBanner>
        )}
        {/* Every section below describes the loaded document, so the gate is
            the document alone: with no document the form would speak for a
            workflow nobody read — no input vars declared, a sandbox badge
            reading "auto (default)" for a `sandbox: none` file whose reload
            failed. The banner above carries the cause. */}
        {!doc ? (
          !error && <div className="text-xs text-fg-subtle">Loading workflow…</div>
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
                    botSlug={namedSlug}
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
              prominentNames={hintedPrimary}
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
                    under Bot options, or launch as-is.
                  </p>
                )
              }
            />
            {/* Everything below is tuning, not launch-blocking: two sibling
                disclosures keep the first-launch page to target + inputs
                (the wizard bar). "Bot options" holds the bot's own optional
                + auto-managed inputs (omitted when it has none); "Engine
                options" holds the iterion knobs — backend, per-node models,
                budget caps, worktree finalization — identical for every
                bot. */}
            <div className="mt-4 space-y-2">
              <BotOptionsSection
                advancedVarFields={advancedVarFields}
                autoManagedFields={autoManagedFields}
                values={values}
                submitting={submitting}
                onValueChange={onValueChange}
                onSubmit={onSubmit}
                open={overrides.botOptionsOpen}
                onToggle={overrides.toggleBotOptions}
              />
              <EngineOptionsSection
                overrides={overrides}
                fields={fields}
                llmNodes={llmNodes}
                worktreeOn={worktreeOn}
                cloud={cloud}
                provider={repoTarget.selectedRepoProvider}
              />
            </div>

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
