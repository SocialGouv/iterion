import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import { useBotsStore } from "@/store/bots";
import * as filesApi from "@/api/client";
import { createForgeRepo } from "@/api/forgeConnections";
import { createRun, getServerInfo, previewRunCost, uploadAttachment } from "@/api/runs";
import type { MergeStrategy, PreviewEffectiveSettings } from "@/api/runs";
import type { AttachmentField, IterDocument, ServerInfo } from "@/api/types";

import { Button } from "@/components/ui/Button";
import { DesktopOnlyNotice } from "@/components/ui/DesktopOnlyNotice";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useDocumentStore } from "@/store/document";
import { useBackendDetectStore } from "@/store/backendDetect";
import { useServerInfoStore } from "@/store/serverInfo";

import { type AttachmentValue } from "./AttachmentFieldInput";
import { defaultStringFor } from "@/components/shared/VarFieldInput";
import { isVarMissing } from "@/lib/varValidation";

import AttachmentsSection from "./launchView/AttachmentsSection";
import BudgetSection from "./launchView/BudgetSection";
import {
  buildBudgetPayload,
  emptyBudgetFieldValues,
  type BudgetFieldValues,
} from "./launchView/budgetPayload";
import LaunchBar from "./launchView/LaunchBar";
import ModelOverridesSection, {
  type LLMNode,
  type NodeOverride,
} from "./launchView/ModelOverridesSection";
import PresetSection from "./launchView/PresetSection";
import RepoTargetSection, {
  findAttachedRepo,
  initialRepoTargetState,
  isRepoTargetValid,
  type RepoTargetState,
} from "./launchView/RepoTargetSection";
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
  // The in-memory editor buffer, used to launch an UNSAVED workflow (no
  // ?file= path). This is the cloud path: the server pod's rootfs is
  // read-only, so /files/save 500s and there is never an on-disk path to
  // reference — the launch API takes inline `source` instead (see
  // resolveWorkflowPath: cloud mode returns the empty file_path and runs
  // off Source). Also lets a fresh local buffer launch before its first
  // save.
  const storeDocument = useDocumentStore((s) => s.document);
  // Pristine-buffer detection: the document store initializes with a
  // default scaffold document (createEmptyDocument), so `storeDocument`
  // is never null — a bare deep-link to /runs/new would otherwise
  // silently offer launching that implicit scaffold as an "Unsaved
  // workflow". A buffer only counts as a real launch candidate once the
  // user opened a file (currentFilePath set) or edited the scaffold
  // (generation moved past the last-saved mark).
  const editorFilePath = useDocumentStore((s) => s.currentFilePath);
  const editorDirty = useDocumentStore(
    (s) => s._generation !== s._savedGeneration,
  );
  const noSource = !filePath && editorFilePath === null && !editorDirty;
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
  // The single Advanced disclosure grouping backend/model/budget/worktree
  // tuning — collapsed on first launch so the page reads target + inputs.
  const [advancedOpen, setAdvancedOpen] = useState(false);
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
  // Server-resolved knob provenance below run-override (workflow/env/
  // default), captioning the Run-settings selects. Best-effort — a
  // parse failure just hides the captions.
  const [effectiveSettings, setEffectiveSettings] =
    useState<PreviewEffectiveSettings | null>(null);
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
  const backendReport = useBackendDetectStore((s) => s.report);
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");

  // Target-repository section — cloud-only. When the bot's manifest declares
  // a `repo:` block (mode != "none"), the operator picks attach/create/skip
  // before launch. State is owned here so the create-then-launch flow can
  // stage a createForgeRepo() call and pipe its output into createRun.
  const {
    activeRepo,
    repos: teamRepos,
    teamID,
    enabled: repoContextEnabled,
    loading: repoContextLoading,
  } = useActiveRepo();
  const [repoTargetState, setRepoTargetState] = useState<RepoTargetState | null>(null);
  const [repoTargetCreateError, setRepoTargetCreateError] = useState<string | null>(null);

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
    // Knob provenance for the Run-settings captions. Best-effort: any
    // failure just leaves the captions hidden.
    if (!filePath) {
      setEffectiveSettings(null);
      return;
    }
    let cancelled = false;
    previewRunCost({ file_path: filePath })
      .then((res) => {
        if (!cancelled) setEffectiveSettings(res.effective ?? null);
      })
      .catch(() => {
        if (!cancelled) setEffectiveSettings(null);
      });
    return () => {
      cancelled = true;
    };
  }, [filePath]);

  useEffect(() => {
    if (!filePath) {
      // No ?file= path — launch the unsaved editor buffer via inline
      // source. The launch API (resolveWorkflowPath) runs off Source when
      // file_path is empty, so this is a first-class path, not a fallback
      // hack; it is the ONLY way to launch in cloud mode, where the pod
      // rootfs is read-only and a workflow can never be saved to disk.
      //
      // The live edit buffer is the document store's AST (currentSource is
      // only the last opened/saved file's text, stale after edits), so we
      // derive the source from it via /api/unparse before launching.
      //
      // A pristine store buffer (never opened, never edited) is NOT a
      // launchable workflow — the render below shows a picker empty
      // state instead, so don't unparse the scaffold here.
      if (noSource) return;
      if (!storeDocument) {
        setError("No workflow to launch — open or write one in the editor first.");
        return;
      }
      let cancelled = false;
      filesApi
        .unparse(storeDocument)
        .then((src) => {
          if (cancelled) return;
          setCurrentSource(src);
          setDoc(storeDocument);
          const fields = pickVars(storeDocument);
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
  }, [filePath, noSource, setCurrentSource, storeDocument]);

  const fields = pickVars(doc);

  // LLM nodes (agents + judges) the operator can retarget per run. Names are
  // the exact node ids used as override selectors. Judges first so the review
  // side reads top-down in the section.
  const llmNodes = useMemo<LLMNode[]>(() => {
    const judges: LLMNode[] = (doc?.judges ?? []).map((j) => ({
      name: j.name,
      kind: "judge",
      model: j.model,
      backend: j.backend,
    }));
    const agents: LLMNode[] = (doc?.agents ?? []).map((a) => ({
      name: a.name,
      kind: "agent",
      model: a.model,
      backend: a.backend,
    }));
    return [...judges, ...agents];
  }, [doc]);

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

  // Show the "Target repository" section when: the bot declares a repo need
  // (mode !== "none"), we're in cloud mode with a team context, and the
  // useActiveRepo hook is wired (repos + activeRepo available).
  const repoRequirement = bot?.repo && bot.repo.mode !== "none" ? bot.repo : null;
  const showRepoTarget =
    !!repoRequirement && serverInfo?.mode === "cloud" && repoContextEnabled;

  // Initialise / re-initialise the repo-target state when the bot changes.
  // Deliberately keyed on the bot's rel_path + the useActiveRepo loading
  // flag so operator edits aren't clobbered by every useActiveRepo refetch,
  // but the initial pick doesn't miss "Use <active repo>" just because the
  // repos query hadn't landed yet. The activeRepo / teamRepos snapshot is
  // captured via closure at the moment the effect fires.
  const botKey = bot?.rel_path ?? "";
  useEffect(() => {
    if (!repoRequirement) {
      setRepoTargetState(null);
      return;
    }
    if (repoContextLoading) return;
    setRepoTargetState(initialRepoTargetState(repoRequirement, activeRepo, teamRepos));
    setRepoTargetCreateError(null);
    // activeRepo / teamRepos intentionally excluded — we seed from the
    // current snapshot then let the operator drive further changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [botKey, repoRequirement?.mode, repoRequirement?.allow_create, repoContextLoading]);

  const repoTargetValid =
    !repoRequirement ||
    !showRepoTarget ||
    (repoTargetState !== null &&
      isRepoTargetValid(repoRequirement, repoTargetState, activeRepo, teamRepos));
  const missingRepoTarget =
    showRepoTarget && repoRequirement?.mode === "required" && !repoTargetValid;

  // First unfilled required field, in precedence order (attachments before
  // vars, then repo target). One walk feeds the launch gate, the caption
  // text, and the scroll/focus target — so the precedence can't drift
  // across them.
  const missingAttachmentField = attachmentFields.find(
    (f) => f.required && !attachments[f.name]?.uploadId,
  );
  const missingVarField = fields.find((f) =>
    isVarMissing(f, values[f.name] ?? ""),
  );
  const missingRequired = !!(missingAttachmentField || missingVarField || missingRepoTarget);
  const missingTitle = missingAttachmentField
    ? "Provide every required attachment first"
    : missingVarField
      ? "Fill every required input first"
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
        review_mode:
          reviewModeOverride && reviewModeOverride !== "auto"
            ? reviewModeOverride
            : undefined,
        budget: buildBudgetPayload(budgetFields),
        model_overrides: (() => {
          const entries = Object.entries(modelOverrides)
            .map(([selector, o]) => ({
              selector,
              model: o.model?.trim() || undefined,
              backend: o.backend || undefined,
            }))
            .filter((e) => e.model || e.backend);
          return entries.length > 0 ? entries : undefined;
        })(),
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

  // Deep-link with nothing to launch (no ?file= and a pristine editor
  // buffer): never offer the implicit empty scaffold — route the user
  // to a real workflow instead. In cloud mode /bots is the launch
  // surface (the raw editor is a power-user detour); local/desktop keeps
  // the editor as its primary entry point.
  if (noSource) {
    return (
      <div className="h-full flex flex-col bg-surface-1 text-fg-default">
        {cloud ? (
          <EmptyState
            title="No workflow to launch"
            message="This page launches a specific workflow, but none is selected. Pick a bot from the gallery, then launch from its home."
            action={
              <Button
                size="sm"
                variant="primary"
                onClick={() => setLocation("/bots")}
              >
                Browse bots
              </Button>
            }
            secondaryAction={
              <Button
                size="sm"
                variant="secondary"
                onClick={() => setLocation("/")}
              >
                Home
              </Button>
            }
          />
        ) : (
          <EmptyState
            title="No workflow to launch"
            message="This page launches a specific workflow, but none is selected. Open a .bot file in the Editor, or pick a bot or recent workflow from Home, then launch from there."
            action={
              <Button
                size="sm"
                variant="primary"
                onClick={() => setLocation("/editor")}
              >
                Open the Editor
              </Button>
            }
            secondaryAction={
              <Button
                size="sm"
                variant="secondary"
                onClick={() => setLocation("/")}
              >
                Browse workflows
              </Button>
            }
          />
        )}
      </div>
    );
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
            {/* The target repository is the launch's primary decision for
                repo-declaring bots — it leads, mirroring the wizard's
                one-decision-first ordering. */}
            {showRepoTarget && repoRequirement && repoTargetState && (
              <div id="repo-target-section">
                <RepoTargetSection
                  repo={repoRequirement}
                  activeRepo={activeRepo}
                  repos={teamRepos}
                  teamID={teamID}
                  state={repoTargetState}
                  onChange={(patch) =>
                    setRepoTargetState((prev) => (prev ? { ...prev, ...patch } : prev))
                  }
                  createError={repoTargetCreateError}
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
              fields={fields}
              attachmentFields={attachmentFields}
              values={values}
              submitting={submitting}
              onValueChange={(name, value) =>
                setValues((prev) => ({ ...prev, [name]: value }))
              }
              onSubmit={onSubmit}
            />
            {/* Everything below is tuning, not launch-blocking: one
                disclosure keeps the first-launch page to target + inputs
                (the wizard bar), while power users open it for backend,
                per-node models, budget caps and worktree finalization. */}
            <div className="mt-4 border-t border-border-subtle pt-2">
              <button
                type="button"
                onClick={() => setAdvancedOpen((v) => !v)}
                aria-expanded={advancedOpen}
                className="flex w-full items-center gap-1.5 py-1 text-xs font-medium text-fg-muted hover:text-fg-default"
              >
                <span aria-hidden>{advancedOpen ? "▾" : "▸"}</span>
                Advanced
                <span className="font-normal text-fg-subtle">
                  backend · per-node models · budget · worktree
                </span>
              </button>
              {advancedOpen && (
                <>
                  <RunSettingsSection
                    backendOverride={backendOverride}
                    compressOverride={compressOverride}
                    permissionOverride={permissionOverride}
                    reviewModeOverride={reviewModeOverride}
                    backendReport={backendReport}
                    effective={effectiveSettings}
                    onBackendChange={setBackendOverride}
                    onCompressChange={setCompressOverride}
                    onPermissionChange={setPermissionOverride}
                    onReviewModeChange={setReviewModeOverride}
                    showReviewMode={fields.some((f) => f.name === "review_mode")}
                  />
                  <ModelOverridesSection
                    nodes={llmNodes}
                    overrides={modelOverrides}
                    backendReport={backendReport}
                    onChange={(name, patch) =>
                      setModelOverrides((prev) => ({
                        ...prev,
                        [name]: { ...prev[name], ...patch },
                      }))
                    }
                  />
                  <BudgetSection
                    show={showBudget}
                    values={budgetFields}
                    onToggle={() => setShowBudget((v) => !v)}
                    onChange={(patch) =>
                      setBudgetFields((prev) => ({ ...prev, ...patch }))
                    }
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
                    cloud={cloud}
                  />
                </>
              )}
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

      <ConfirmDialog
        open={showNoSandboxConfirm}
        title="Launch without sandbox?"
        message={
          cloud ? (
            <>
              <p>
                This workflow doesn't declare a <code>sandbox:</code>{" "}
                block, so its tools and shell commands run directly in
                the ephemeral runner pod's filesystem — no container
                isolation between the bot and the runner's mounted
                credentials, workspace clone, or outbound network egress.
              </p>
              <p>
                Add <code>sandbox: auto</code> (devcontainer-aware) or
                an inline block with an image in the workflow file to
                narrow the write scope and the tools the bot can reach.
              </p>
            </>
          ) : (
            <>
              <p>
                This workflow doesn't declare a <code>sandbox:</code>{" "}
                block, so its tools and shell commands will run directly
                on the host. The bot can read, modify, or delete any
                file the iterion process has access to.
              </p>
              <p>
                Add <code>sandbox: auto</code> (devcontainer-aware) or
                an inline block with an image in the workflow file to
                opt into container isolation.
              </p>
            </>
          )
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
