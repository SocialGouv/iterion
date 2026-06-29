import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import { useBotsStore } from "@/store/bots";
import * as filesApi from "@/api/client";
import { createRun, getServerInfo, uploadAttachment } from "@/api/runs";
import type { MergeStrategy } from "@/api/runs";
import type { AttachmentField, IterDocument, ServerInfo } from "@/api/types";

import { Button } from "@/components/ui/Button";
import { DesktopOnlyNotice } from "@/components/ui/DesktopOnlyNotice";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useDocumentStore } from "@/store/document";
import { useBackendDetectStore } from "@/store/backendDetect";

import { type AttachmentValue } from "./AttachmentFieldInput";
import { defaultStringFor } from "@/components/shared/VarFieldInput";
import { isVarMissing } from "@/lib/varValidation";

import AttachmentsSection from "./launchView/AttachmentsSection";
import LaunchBar from "./launchView/LaunchBar";
import PresetSection from "./launchView/PresetSection";
import RunSettingsSection from "./launchView/RunSettingsSection";
import VarFieldsSection from "./launchView/VarFieldsSection";
import WorktreeFinalizationSection from "./launchView/WorktreeFinalizationSection";
import {
  isSandboxActive,
  literalToString,
  pickAttachments,
  pickPresets,
  pickVars,
  sandboxModeLabel,
} from "./launchView/utils";

export default function LaunchView() {
  const [, setLocation] = useLocation();
  const search = useSearch();
  const filePath = useMemo(() => {
    const params = new URLSearchParams(search);
    return params.get("file") ?? "";
  }, [search]);

  const [doc, setDoc] = useState<IterDocument | null>(null);
  const currentSource = useDocumentStore((s) => s.currentSource);
  const setCurrentSource = useDocumentStore((s) => s.setCurrentSource);
  const [values, setValues] = useState<Record<string, string>>({});
  const [selectedPreset, setSelectedPreset] = useState<string>("");
  const [attachments, setAttachments] = useState<Record<string, AttachmentValue | null>>({});
  const [serverInfo, setServerInfo] = useState<ServerInfo | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  // Set when the user clicks Launch on a workflow with no sandbox
  // active. Surfaces the ConfirmDialog so they make a deliberate
  // choice (host execution carries real risk: any tool the bot calls
  // runs against the operator's machine).
  const [showNoSandboxConfirm, setShowNoSandboxConfirm] = useState(false);
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
  // Backend override for this run. "" = let the resolver pick (the
  // current behaviour, mirrored in Settings → Backends).
  // Sending an explicit name overrides the workflow's `default_backend:`
  // but node-level explicit `backend:` still wins.
  const [backendOverride, setBackendOverride] = useState<string>("");
  // command-output-compression override for this run ("" inherits the
  // workflow/node `compress:` DSL then ITERION_COMPRESS).
  const [compressOverride, setCompressOverride] = useState<string>("");
  // tool-permission gate mode override ("" inherits the workflow/node
  // `permission:` DSL then ITERION_PERMISSION).
  const [permissionOverride, setPermissionOverride] = useState<string>("");
  const backendReport = useBackendDetectStore((s) => s.report);

  useEffect(() => {
    let cancelled = false;
    getServerInfo()
      .then((info) => {
        if (!cancelled) setServerInfo(info);
      })
      .catch(() => {
        // Non-fatal: limits remain unknown; UI shows no bandeau and the
        // server still rejects oversized uploads on the wire.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    if (!filePath) {
      setError("Missing ?file=<path> query parameter");
      return;
    }
    let cancelled = false;
    filesApi
      .openFile(filePath)
      .then((res) => {
        if (cancelled) return;
        setDoc(res.document);
        setCurrentSource(res.source);
        const fields = pickVars(res.document);
        const initial: Record<string, string> = {};
        for (const f of fields) initial[f.name] = defaultStringFor(f);
        setValues(initial);
      })
      .catch((e) => {
        if (!cancelled) setError(errorMessage(e));
      });
    return () => {
      cancelled = true;
    };
  }, [filePath, setCurrentSource]);

  const fields = pickVars(doc);

  // Prefer the bot schema's presets (the union of in-source `presets:` and
  // file-based presets/<name>.md, carrying display_name / description / prompt
  // / skills) when the open file is a bundle's main.bot; fall back to the
  // workflow doc's in-source presets for a loose .bot file.
  const allBots = useBotsStore((s) => s.bots);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    if (allBots === null) void fetchBots();
  }, [allBots, fetchBots]);
  const bot = useMemo(
    () =>
      allBots?.find(
        (b) => b.is_bundle && b.rel_path && filePath === `${b.rel_path}/main.bot`,
      ) ?? null,
    [allBots, filePath],
  );
  const presets = bot?.presets?.entries ?? pickPresets(doc);
  const selectedPresetMeta = useMemo(
    () => presets.find((p) => p.name === selectedPreset),
    [presets, selectedPreset],
  );
  const attachmentFields = pickAttachments(doc);
  const limits = serverInfo?.limits.upload ?? null;

  // Apply a named preset by overlaying its values onto the current form
  // state. Existing values for keys not in the preset are preserved, so
  // switching from "prod" to "dev" updates only the overlapping keys —
  // which is the same precedence as the engine.
  const applyPreset = (name: string) => {
    setSelectedPreset(name);
    if (!name) return;
    const preset = presets.find((p) => p.name === name);
    if (!preset) return;
    setValues((prev) => {
      const next = { ...prev };
      for (const pv of preset.values) {
        next[pv.key] = literalToString(pv.value);
      }
      return next;
    });
  };

  // First unfilled required field, in precedence order (attachments before
  // vars). One walk feeds the launch gate, the caption text, and the
  // scroll/focus target — so the precedence can't drift across them.
  const missingAttachmentField = attachmentFields.find(
    (f) => f.required && !attachments[f.name]?.uploadId,
  );
  const missingVarField = fields.find((f) =>
    isVarMissing(f, values[f.name] ?? ""),
  );
  const missingRequired = !!(missingAttachmentField || missingVarField);
  const missingTitle = missingAttachmentField
    ? "Provide every required attachment first"
    : missingVarField
      ? "Fill every required input first"
      : undefined;
  const firstMissingFieldId = missingAttachmentField
    ? `attach-${missingAttachmentField.name}`
    : missingVarField
      ? `var-${missingVarField.name}`
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

  // Auto-upload as soon as a file is selected. The upload runs in the
  // background and the launch button stays disabled until every entry
  // either has an uploadId or is optional and absent.
  const handleAttachmentChange = async (
    field: AttachmentField,
    next: AttachmentValue | null,
  ) => {
    setAttachments((prev) => ({ ...prev, [field.name]: next }));
    if (!next || next.error || next.uploadId) return;
    // Kick off the upload.
    setAttachments((prev) => ({
      ...prev,
      [field.name]: { ...next, progress: 0 },
    }));
    // Throttle progress updates to once per percentage step. XHR can
    // emit progress 100+ times per second on a fast pipe; coalescing
    // here keeps the re-render budget bounded to ~100 per attachment.
    let lastPct = -1;
    try {
      const staged = await uploadAttachment(next.file, {
        declaredMime: next.file.type || undefined,
        onProgress: (loaded, total) => {
          const frac = total > 0 ? loaded / total : 0;
          const pct = Math.floor(frac * 100);
          if (pct === lastPct) return;
          lastPct = pct;
          setAttachments((prev) => {
            const cur = prev[field.name];
            if (!cur || cur.file !== next.file) return prev;
            return {
              ...prev,
              [field.name]: { ...cur, progress: frac },
            };
          });
        },
      });
      setAttachments((prev) => {
        const cur = prev[field.name];
        if (!cur || cur.file !== next.file) return prev;
        return {
          ...prev,
          [field.name]: {
            ...cur,
            uploadId: staged.upload_id,
            progress: undefined,
          },
        };
      });
    } catch (err) {
      setAttachments((prev) => {
        const cur = prev[field.name];
        if (!cur || cur.file !== next.file) return prev;
        return {
          ...prev,
          [field.name]: {
            ...cur,
            error: errorMessage(err),
            progress: undefined,
          },
        };
      });
    }
  };

  // launchRun runs the actual createRun call. Separated from the
  // user-facing onSubmit so the no-sandbox ConfirmDialog can reach it
  // directly when the user accepts the warning.
  const launchRun = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const attachmentsPayload: Record<string, string> = {};
      for (const f of attachmentFields) {
        const a = attachments[f.name];
        if (a?.uploadId) attachmentsPayload[f.name] = a.uploadId;
      }
      const res = await createRun({
        file_path: filePath,
        source: currentSource || undefined,
        vars: values,
        preset: selectedPreset || undefined,
        merge_into: mergeInto || undefined,
        branch_name: branchName || undefined,
        merge_strategy: mergeStrategy,
        auto_merge: autoMerge,
        attachments:
          Object.keys(attachmentsPayload).length > 0 ? attachmentsPayload : undefined,
        backend: backendOverride || undefined,
        compress: compressOverride || undefined,
        permission: permissionOverride || undefined,
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

  // Surface the worktree config so the user knows whether the
  // finalization fields will have any effect. Only the first workflow
  // is inspected (matches pickVars's selection rule).
  const worktreeMode = doc?.workflows?.[0]?.worktree ?? "";
  const worktreeOn = worktreeMode === "auto";

  // Auto-open the worktree-finalization block once the document loads
  // and the workflow uses worktree:auto. Done in an effect so it only
  // fires after `doc` is populated and doesn't fight a user who
  // explicitly closed the section afterwards (we only flip false→true).
  useEffect(() => {
    if (worktreeOn) setShowAdvanced(true);
  }, [worktreeOn]);

  useHeaderSlot({
    left: (
      <>
        <span className="text-xs font-semibold text-fg-muted">Launch run</span>
        <span className="text-xs text-fg-subtle font-mono truncate max-w-md" title={filePath}>
          {filePath}
        </span>
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
              fields={fields}
              attachmentFields={attachmentFields}
              values={values}
              submitting={submitting}
              onValueChange={(name, value) =>
                setValues((prev) => ({ ...prev, [name]: value }))
              }
              onSubmit={onSubmit}
            />
            <RunSettingsSection
              backendOverride={backendOverride}
              compressOverride={compressOverride}
              permissionOverride={permissionOverride}
              backendReport={backendReport}
              onBackendChange={setBackendOverride}
              onCompressChange={setCompressOverride}
              onPermissionChange={setPermissionOverride}
            />
            <WorktreeFinalizationSection
              showAdvanced={showAdvanced}
              worktreeOn={worktreeOn}
              mergeInto={mergeInto}
              branchName={branchName}
              mergeStrategy={mergeStrategy}
              autoMerge={autoMerge}
              onToggle={() => setShowAdvanced((v) => !v)}
              onMergeIntoChange={setMergeInto}
              onBranchNameChange={setBranchName}
              onMergeStrategyChange={setMergeStrategy}
              onAutoMergeChange={setAutoMerge}
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

      <ConfirmDialog
        open={showNoSandboxConfirm}
        title="Launch without sandbox?"
        message={
          <>
            <p>
              This workflow doesn't declare a <code>sandbox:</code> block,
              so its tools and shell commands will run directly on the
              host. The bot can read, modify, or delete any file the
              iterion process has access to.
            </p>
            <p>
              Add <code>sandbox: auto</code> (devcontainer-aware) or an
              inline block with an image in the workflow file to opt into
              container isolation.
            </p>
          </>
        }
        confirmLabel="Launch unsandboxed"
        confirmVariant="danger"
        secondaryAction={{
          label: "Edit workflow first",
          onClick: () => {
            setShowNoSandboxConfirm(false);
            setLocation("/editor");
          },
        }}
        onConfirm={() => {
          setShowNoSandboxConfirm(false);
          void launchRun();
        }}
        onCancel={() => setShowNoSandboxConfirm(false)}
      />
    </div>
  );
}
