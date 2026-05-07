import { useEffect, useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import * as filesApi from "@/api/client";
import { createRun, getServerInfo, uploadAttachment } from "@/api/runs";
import type { MergeStrategy } from "@/api/runs";
import type {
  AttachmentField,
  IterDocument,
  ServerInfo,
  VarField,
} from "@/api/types";
import { Button } from "@/components/ui/Button";
import { Select } from "@/components/ui/Select";
import { useDocumentStore } from "@/store/document";

import AttachmentFieldInput, {
  type AttachmentValue,
} from "./AttachmentFieldInput";
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

/** Read the workflow's attachments — same precedence as vars. */
function pickAttachments(doc: IterDocument | null): AttachmentField[] {
  if (!doc) return [];
  const wf = doc.workflows?.[0];
  if (wf?.attachments?.fields?.length) return wf.attachments.fields;
  return doc.attachments?.fields ?? [];
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
  const [attachments, setAttachments] = useState<Record<string, AttachmentValue | null>>({});
  const [serverInfo, setServerInfo] = useState<ServerInfo | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
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
  const attachmentFields = pickAttachments(doc);
  const limits = serverInfo?.limits.upload ?? null;

  // Required attachments must have a successful upload (uploadId present).
  const missingRequired = attachmentFields.some(
    (f) => f.required && !attachments[f.name]?.uploadId,
  );

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
    try {
      const staged = await uploadAttachment(next.file, {
        declaredMime: next.file.type || undefined,
        onProgress: (loaded, total) => {
          setAttachments((prev) => {
            const cur = prev[field.name];
            if (!cur || cur.file !== next.file) return prev;
            return {
              ...prev,
              [field.name]: { ...cur, progress: total > 0 ? loaded / total : 0 },
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

  const onSubmit = async () => {
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
      <header className="border-b border-border-default px-4 py-3 flex items-center gap-3">
        <h1 className="text-sm font-bold">Launch run</h1>
        <span className="text-xs text-fg-subtle font-mono truncate">{filePath}</span>
        <button
          className="ml-auto text-xs px-2 py-1 rounded bg-surface-2 hover:bg-surface-3"
          onClick={() => setLocation("/edit")}
        >
          Cancel
        </button>
      </header>

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
                    const noDefault = !f.default;
                    if (promptLike) {
                      return (
                        <div key={f.name} className="flex flex-col gap-1.5">
                          <label htmlFor={`var-${f.name}`} className="flex items-baseline gap-2">
                            <span className="text-xs font-medium font-mono text-fg-default">{f.name}</span>
                            <span className="text-[10px] text-fg-subtle">{f.type}</span>
                            {noDefault && (
                              <span className="text-[10px] text-warning-fg uppercase tracking-wide">required</span>
                            )}
                          </label>
                          <VarFieldInput
                            field={f}
                            value={values[f.name] ?? ""}
                            onChange={(v) =>
                              setValues((prev) => ({ ...prev, [f.name]: v }))
                            }
                          />
                        </div>
                      );
                    }
                    return (
                      <div key={f.name} className="grid grid-cols-[160px_1fr] gap-3 items-start">
                        <label htmlFor={`var-${f.name}`} className="pt-1">
                          <div className="text-xs font-medium font-mono">{f.name}</div>
                          <div className="text-[10px] text-fg-subtle">{f.type}</div>
                        </label>
                        <VarFieldInput
                          field={f}
                          value={values[f.name] ?? ""}
                          onChange={(v) =>
                            setValues((prev) => ({ ...prev, [f.name]: v }))
                          }
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

            <div className="mt-6 flex items-center gap-2">
              <Button
                variant="primary"
                onClick={() => void onSubmit()}
                disabled={submitting || !doc || missingRequired}
                title={missingRequired ? "Provide every required attachment first" : undefined}
              >
                {submitting ? "Launching…" : "Launch"}
              </Button>
              <span className="text-[10px] text-fg-subtle">
                Run ID is generated automatically.
              </span>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
