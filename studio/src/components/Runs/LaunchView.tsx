import { useEffect, useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import * as filesApi from "@/api/client";
import { createRun, getServerInfo, uploadAttachment } from "@/api/runs";
import type { MergeStrategy } from "@/api/runs";
import type {
  AttachmentField,
  IterDocument,
  Literal,
  Preset,
  ServerInfo,
  VarField,
} from "@/api/types";
import { CheckCircledIcon, ExclamationTriangleIcon } from "@radix-ui/react-icons";

import { Button } from "@/components/ui/Button";
import { Select } from "@/components/ui/Select";
import AppHeader from "@/components/shared/AppHeader";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useDocumentStore } from "@/store/document";

import AttachmentFieldInput, {
  type AttachmentValue,
} from "./AttachmentFieldInput";
import CostPreviewChip from "./CostPreviewChip";
import VarFieldInput, { defaultStringFor } from "./VarFieldInput";
import { isPromptLikeVar } from "@/lib/promptVarHeuristics";
import { formatBytes, totalSize } from "@/lib/attachmentValidation";

/** Read the workflow's vars (workflow-level if a single workflow is
 *  declared, else the file-level `vars:` block). */
function pickVars(doc: IterDocument | null): VarField[] {
  if (!doc) return [];
  const wf = doc.workflows?.[0];
  if (wf?.vars?.fields?.length) return wf.vars.fields;
  return doc.vars?.fields ?? [];
}

/** A var is required when the workflow declares no default. Bool fields
 *  always have an effective default ("false"), so they're never missing. */
function isVarRequired(field: VarField): boolean {
  if (field.type === "bool") return false;
  return !field.default;
}

function isVarMissing(field: VarField, value: string): boolean {
  if (!isVarRequired(field)) return false;
  return value.trim().length === 0;
}

/** Small reused affordance — the "required" pill next to a field label. */
function RequiredPill() {
  return (
    <span className="text-[10px] text-warning-fg uppercase tracking-wide">required</span>
  );
}

/** Read the workflow's presets (top-level only — they apply to the
 *  whole file, no workflow-level scope today). */
function pickPresets(doc: IterDocument | null): Preset[] {
  return doc?.presets?.entries ?? [];
}

/** Stringify a preset literal so it can feed the existing var form
 *  (which holds every value as a string and coerces server-side). */
function literalToString(lit: Literal | undefined): string {
  if (!lit) return "";
  switch (lit.kind) {
    case "string":
      return lit.str_val ?? "";
    case "int":
      return String(lit.int_val ?? 0);
    case "float":
      return String(lit.float_val ?? 0);
    case "bool":
      return lit.bool_val ? "true" : "false";
    default:
      return lit.raw ?? "";
  }
}

/** Read the workflow's attachments — same precedence as vars. */
function pickAttachments(doc: IterDocument | null): AttachmentField[] {
  if (!doc) return [];
  const wf = doc.workflows?.[0];
  if (wf?.attachments?.fields?.length) return wf.attachments.fields;
  return doc.attachments?.fields ?? [];
}

/** isSandboxActive mirrors pkg/dsl/ir/sandbox.go SandboxSpec.IsActive:
 *  the workflow declares a sandbox block whose mode is "auto" or
 *  "inline". Absent block or mode: "none" → host runs the tools. */
function isSandboxActive(doc: IterDocument | null): boolean {
  const sb = doc?.workflows?.[0]?.sandbox;
  if (!sb) return false;
  const m = (sb.mode ?? "").toLowerCase();
  return m === "auto" || m === "inline";
}

/** sandboxModeLabel returns "auto" / "inline" / "none" / "" — the empty
 *  string when no block is declared. Used by the SandboxBadge so the
 *  badge label tracks the IR's view of the workflow without re-parsing. */
function sandboxModeLabel(doc: IterDocument | null): string {
  const sb = doc?.workflows?.[0]?.sandbox;
  if (!sb) return "";
  return (sb.mode ?? "").toLowerCase();
}

// SandboxBadge surfaces the workflow's sandbox isolation level next to
// the Launch button so the operator never confirms a host-execution run
// by accident. Three states match pkg/dsl/ir/sandbox.go SandboxSpec:
//   auto / inline → green "sandboxed" pill
//   none          → red "host execution" pill
//   (no block)    → red "no sandbox" pill, same risk as `none`
// The badge title carries the long-form description so the chip itself
// stays compact in the Launch row.
function SandboxBadge({ mode }: { mode: string }) {
  const active = mode === "auto" || mode === "inline";
  const label = active
    ? `Sandbox: ${mode}`
    : mode === "none"
    ? "Sandbox: none"
    : "No sandbox";
  const cls = active
    ? "bg-success-soft text-success-fg border-success/40"
    : "bg-danger-soft text-danger-fg border-danger/40";
  const title = active
    ? "Workflow declares a sandbox block — tools run inside the container."
    : "Workflow has no active sandbox — tools run directly on the host. Add `sandbox: auto` for isolation.";
  return (
    <span
      className={`inline-flex items-center gap-1 text-[10px] px-1.5 py-0.5 rounded border ${cls}`}
      title={title}
    >
      {active ? (
        <CheckCircledIcon className="w-3 h-3" aria-hidden="true" />
      ) : (
        <ExclamationTriangleIcon className="w-3 h-3" aria-hidden="true" />
      )}
      {label}
    </span>
  );
}

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
        if (!cancelled) setError((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, [filePath]);

  const fields = pickVars(doc);
  const presets = pickPresets(doc);
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

  // Required attachments must have a successful upload (uploadId present).
  const missingAttachment = attachmentFields.some(
    (f) => f.required && !attachments[f.name]?.uploadId,
  );
  // Required vars (no default declared) must have a non-blank value.
  const missingVar = fields.some((f) => isVarMissing(f, values[f.name] ?? ""));
  const missingRequired = missingAttachment || missingVar;
  const missingTitle = missingAttachment
    ? "Provide every required attachment first"
    : missingVar
      ? "Fill every required input first"
      : undefined;

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
            error: (err as Error).message,
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
      });
      setLocation(`/runs/${encodeURIComponent(res.run_id)}`);
    } catch (e) {
      setError((e as Error).message);
      setSubmitting(false);
    }
  };

  // onSubmit is the click target. Intercepts the launch when the
  // workflow has no sandbox declared and opens the ConfirmDialog;
  // otherwise calls launchRun directly.
  const onSubmit = () => {
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

  return (
    <div className="h-full flex flex-col bg-surface-1 text-fg-default">
      <AppHeader
        active="runs"
        rightActions={
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setLocation("/editor")}
          >
            Cancel
          </Button>
        }
      >
        <span className="text-xs font-semibold text-fg-muted">Launch run</span>
        <span className="text-xs text-fg-subtle font-mono truncate max-w-md" title={filePath}>
          {filePath}
        </span>
      </AppHeader>

      <div className="flex-1 overflow-auto px-4 py-4 max-w-3xl">
        {error && (
          <div className="mb-3 px-3 py-2 rounded bg-danger-soft text-danger-fg text-xs">{error}</div>
        )}
        {!doc && !error ? (
          <div className="text-xs text-fg-subtle">Loading workflow…</div>
        ) : (
          <>
            {attachmentFields.length > 0 && (
              <section className="mb-6">
                <h2 className="text-xs font-medium text-fg-muted mb-2">Attachments</h2>
                {limits && (
                  <p className="mb-3 text-[10px] text-fg-subtle font-mono">
                    Max {formatBytes(limits.max_file_size)} per file ·{" "}
                    {formatBytes(limits.max_total_size)} total · up to{" "}
                    {limits.max_files_per_run} files ·{" "}
                    {limits.allowed_mime.slice(0, 4).join(" ")}
                    {limits.allowed_mime.length > 4 ? " …" : ""}
                  </p>
                )}
                <div className="space-y-4">
                  {attachmentFields.map((f) => (
                    <div key={f.name} className="grid grid-cols-[160px_1fr] gap-3 items-start">
                      <label className="pt-1">
                        <div className="text-xs font-medium font-mono">{f.name}</div>
                        <div className="text-[10px] text-fg-subtle">
                          {f.type}
                          {f.required ? " · required" : ""}
                        </div>
                      </label>
                      <AttachmentFieldInput
                        field={f}
                        value={attachments[f.name] ?? null}
                        onChange={(next) => void handleAttachmentChange(f, next)}
                        serverLimits={limits}
                        disabled={submitting}
                      />
                    </div>
                  ))}
                </div>
                {Object.values(attachments).some((a) => a?.file) && (
                  <p className="mt-2 text-[10px] text-fg-subtle">
                    {Object.values(attachments).filter((a) => a?.file).length} file(s),{" "}
                    {formatBytes(totalSize(attachments))} total
                  </p>
                )}
              </section>
            )}
            {presets.length > 0 && (
              <section className="mb-6">
                <div className="flex items-center justify-between mb-2">
                  <h2 className="text-xs font-medium text-fg-muted">Preset</h2>
                  {filePath && (
                    <button
                      type="button"
                      onClick={() =>
                        setLocation(
                          `/editor?file=${encodeURIComponent(filePath)}&focus=presets`,
                        )
                      }
                      className="text-[10px] text-fg-subtle hover:text-fg-default underline"
                      title="Edit presets in the workflow editor"
                    >
                      edit in editor →
                    </button>
                  )}
                </div>
                <Select
                  value={selectedPreset}
                  onChange={(e) => applyPreset(e.target.value)}
                  disabled={submitting}
                >
                  <option value="">— none —</option>
                  {presets.map((p) => (
                    <option key={p.name} value={p.name}>
                      {p.name}
                    </option>
                  ))}
                </Select>
                <p className="mt-1 text-[10px] text-fg-subtle">
                  Selecting a preset overlays its values onto the inputs
                  below. Any further edits override the preset; the engine
                  applies the same precedence (preset &lt; vars).
                </p>
              </section>
            )}
            {fields.length === 0 ? (
              attachmentFields.length === 0 && (
                <p className="text-xs text-fg-subtle">
                  This workflow declares no input vars. You can launch it as-is.
                </p>
              )
            ) : (
              <form
                onSubmit={(e) => {
                  e.preventDefault();
                  if (!submitting) void onSubmit();
                }}
              >
                <h2 className="text-xs font-medium text-fg-muted mb-2">Inputs</h2>
                <div className="space-y-4">
                  {fields.map((f) => {
                    const promptLike = isPromptLikeVar(f);
                    const required = isVarRequired(f);
                    const value = values[f.name] ?? "";
                    const invalid = required && value.trim().length === 0;
                    if (promptLike) {
                      return (
                        <div key={f.name} className="flex flex-col gap-1.5">
                          <label htmlFor={`var-${f.name}`} className="flex items-baseline gap-2">
                            <span className="text-xs font-medium font-mono text-fg-default">{f.name}</span>
                            <span className="text-[10px] text-fg-subtle">{f.type}</span>
                            {required && <RequiredPill />}
                          </label>
                          <VarFieldInput
                            field={f}
                            value={value}
                            onChange={(v) =>
                              setValues((prev) => ({ ...prev, [f.name]: v }))
                            }
                            required={required}
                            invalid={invalid}
                          />
                        </div>
                      );
                    }
                    return (
                      <div key={f.name} className="grid grid-cols-[160px_1fr] gap-3 items-start">
                        <label htmlFor={`var-${f.name}`} className="pt-1">
                          <div className="flex items-baseline gap-2">
                            <span className="text-xs font-medium font-mono">{f.name}</span>
                            {required && <RequiredPill />}
                          </div>
                          <div className="text-[10px] text-fg-subtle">{f.type}</div>
                        </label>
                        <VarFieldInput
                          field={f}
                          value={value}
                          onChange={(v) =>
                            setValues((prev) => ({ ...prev, [f.name]: v }))
                          }
                          required={required}
                          invalid={invalid}
                        />
                      </div>
                    );
                  })}
                </div>
              </form>
            )}
            <div className="mt-6 border-t border-border-default pt-4">
              <button
                type="button"
                className="text-xs text-fg-muted hover:text-fg-default flex items-center gap-1"
                onClick={() => setShowAdvanced((v) => !v)}
              >
                <span>{showAdvanced ? "▼" : "▶"}</span>
                <span>Worktree finalization (squash / merge)</span>
                {!worktreeOn && (
                  <span className="text-[10px] text-fg-subtle">
                    — workflow has no `worktree: auto`, fields are ignored
                  </span>
                )}
              </button>
              {showAdvanced && (
                <div className="mt-3 space-y-3 pl-4 border-l border-border-default">
                  <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
                    <label htmlFor="launch-merge-into" className="pt-1">
                      <div className="text-xs font-medium font-mono">merge_into</div>
                      <div className="text-[10px] text-fg-subtle">
                        FF target after run
                      </div>
                    </label>
                    <div>
                      <input
                        id="launch-merge-into"
                        type="text"
                        className="w-full px-2 py-1 text-xs font-mono rounded bg-surface-2 border border-border-default focus:outline-none focus:ring-1 focus:ring-accent"
                        placeholder="current (default) | none | <branch-name>"
                        value={mergeInto}
                        onChange={(e) => setMergeInto(e.target.value)}
                      />
                      <div className="mt-1 text-[10px] text-fg-subtle">
                        Empty/<code>current</code>: fast-forward your current branch.
                        <code> none</code>: keep commits on the storage branch only.
                        Named branch: only honoured if it matches your checked-out
                        branch.
                      </div>
                    </div>
                  </div>
                  <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
                    <label htmlFor="launch-branch-name" className="pt-1">
                      <div className="text-xs font-medium font-mono">branch_name</div>
                      <div className="text-[10px] text-fg-subtle">Storage branch</div>
                    </label>
                    <div>
                      <input
                        id="launch-branch-name"
                        type="text"
                        className="w-full px-2 py-1 text-xs font-mono rounded bg-surface-2 border border-border-default focus:outline-none focus:ring-1 focus:ring-accent"
                        placeholder="iterion/run/<friendly> (default)"
                        value={branchName}
                        onChange={(e) => setBranchName(e.target.value)}
                      />
                      <div className="mt-1 text-[10px] text-fg-subtle">
                        Override the GC-guard branch name. On collision a numeric
                        suffix is appended.
                      </div>
                    </div>
                  </div>
                  <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
                    <label htmlFor="launch-merge-strategy" className="pt-1">
                      <div className="text-xs font-medium font-mono">merge_strategy</div>
                      <div className="text-[10px] text-fg-subtle">
                        Squash vs merge commit
                      </div>
                    </label>
                    <div>
                      <Select
                        id="launch-merge-strategy"
                        value={mergeStrategy}
                        onChange={(e) =>
                          setMergeStrategy(e.target.value as MergeStrategy)
                        }
                      >
                        <option value="squash">Squash and merge (default)</option>
                        <option value="merge">Merge commit (preserve history)</option>
                      </Select>
                      <div className="mt-1 text-[10px] text-fg-subtle">
                        Used when the run is merged into the target branch — at
                        end of run if auto_merge is on, otherwise from the
                        Commits tab. The fast-forward path is used for "merge".
                      </div>
                    </div>
                  </div>
                  <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
                    <label htmlFor="launch-auto-merge" className="pt-1">
                      <div className="text-xs font-medium font-mono">auto_merge</div>
                      <div className="text-[10px] text-fg-subtle">
                        GitLab-style auto-merge
                      </div>
                    </label>
                    <div className="pt-1">
                      <label className="inline-flex items-center gap-2 text-xs">
                        <input
                          id="launch-auto-merge"
                          type="checkbox"
                          checked={autoMerge}
                          onChange={(e) => setAutoMerge(e.target.checked)}
                        />
                        <span>Auto-merge when run finishes</span>
                      </label>
                      <div className="mt-1 text-[10px] text-fg-subtle">
                        Off (default): commits land on the storage branch only;
                        pick the strategy later from the run's Commits tab —
                        GitHub-PR style. On: the engine applies merge_strategy
                        synchronously at end of run.
                      </div>
                    </div>
                  </div>
                </div>
              )}
            </div>

            <div className="mt-6 flex items-center gap-2 flex-wrap">
              <Button
                variant="primary"
                onClick={onSubmit}
                loading={submitting}
                disabled={!doc || missingRequired}
                title={missingTitle}
              >
                Launch
              </Button>
              <SandboxBadge mode={sandboxModeLabel(doc)} />
              <CostPreviewChip filePath={filePath} source={currentSource || undefined} />
              <span className="text-[10px] text-fg-subtle">
                Run ID is generated automatically.
              </span>
            </div>
          </>
        )}
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
