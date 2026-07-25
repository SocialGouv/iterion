import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { errorMessage } from "@/lib/errorHints";
import {
  getEditorConfig,
  patchEditorConfig,
  type EditorConfigResponse,
  type EditorShare,
} from "@/api/configEditor";
import { Button, Card, EmptyState, InlineBanner, Spinner } from "@/components/ui";

import {
  buildPatch,
  fieldChanged,
  initDraft,
  walkEditableFields,
  type Draft,
  type EditableField,
  type FieldValue,
} from "./fieldModel";
import { CadenceCard } from "./CadenceCard";
import { RecentRunsCard } from "./RecentRunsCard";
import { StringField, ArrayField } from "./ShareFields";
import { ConflictDialog } from "./ConflictDialog";

// ---------------------------------------------------------------------------
// ShareEditor — load one share's projected config, edit its fields, save.
// Remounted (key={share.id}) on selection so all state resets cleanly.
// ---------------------------------------------------------------------------

export type SaveStatus =
  | { kind: "idle" }
  | { kind: "saved"; changed: number }
  | { kind: "error"; message: string };

export function ShareEditor({ teamID, share }: { teamID: string; share: EditorShare }) {
  const [meta, setMeta] = useState<EditorConfigResponse | null>(null);
  const [fields, setFields] = useState<EditableField[]>([]);
  const [baseline, setBaseline] = useState<Draft>({});
  const [draft, setDraft] = useState<Draft>({});
  const [sha, setSha] = useState("");
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [status, setStatus] = useState<SaveStatus>({ kind: "idle" });
  const [conflict, setConflict] = useState<{ serverDraft: Draft; serverSha: string } | null>(
    null,
  );
  const bootRef = useRef(false);

  const bootstrap = useCallback(async () => {
    setLoadError(null);
    try {
      const cfg = await getEditorConfig(teamID, share.id);
      const fs = walkEditableFields(cfg.config);
      const d = initDraft(fs, cfg.config);
      setMeta(cfg);
      setFields(fs);
      setBaseline(d);
      setDraft(d);
      setSha(cfg.sha);
    } catch (err) {
      setLoadError(errorMessage(err));
    }
  }, [teamID, share.id]);

  useEffect(() => {
    if (bootRef.current) return;
    bootRef.current = true;
    void bootstrap();
  }, [bootstrap]);

  const readOnly = meta?.read_only ?? share.read_only;
  const dirty = useMemo(
    () => fields.some((f) => fieldChanged(f, draft, baseline)),
    [fields, draft, baseline],
  );

  const setField = useCallback((path: string, value: FieldValue) => {
    setDraft((prev) => ({ ...prev, [path]: value }));
    setStatus({ kind: "idle" });
  }, []);

  const onSave = useCallback(
    async (forcedSha?: string) => {
      if (readOnly) return;
      const patch = buildPatch(fields, draft, baseline);
      if (Object.keys(patch).length === 0) return;
      setSaving(true);
      setStatus({ kind: "idle" });
      try {
        const result = await patchEditorConfig(teamID, share.id, {
          sha: forcedSha ?? sha,
          patch,
        });
        if (result.kind === "conflict") {
          setConflict({
            serverDraft: initDraft(fields, result.config),
            serverSha: result.sha,
          });
          setStatus({
            kind: "error",
            message: "Someone else edited this file — review the differences before saving.",
          });
          return;
        }
        if (result.kind === "not_editable") {
          setStatus({ kind: "error", message: result.message });
          return;
        }
        setBaseline({ ...draft });
        setSha(result.sha);
        setStatus({ kind: "saved", changed: result.changed.length });
        setConflict(null);
      } catch (err) {
        setStatus({ kind: "error", message: errorMessage(err) });
      } finally {
        setSaving(false);
      }
    },
    [readOnly, fields, draft, baseline, teamID, share.id, sha],
  );

  if (loadError) {
    return (
      <InlineBanner tone="danger" layout="inline" title="Couldn't load this config-share">
        {loadError}
        <div className="mt-2">
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              bootRef.current = false;
              void bootstrap();
            }}
          >
            Try again
          </Button>
        </div>
      </InlineBanner>
    );
  }

  if (!meta) {
    return (
      <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
        <Spinner /> Loading configuration…
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <InlineBanner
        tone="info"
        layout="inline"
        title={`Scoped editor — ${share.label || share.bot_id || share.id}`}
      >
        Editing {share.category ? <strong>{share.category}</strong> : "the config"} of{" "}
        <code className="font-mono">{meta.config_path || share.config_path}</code>. Only the
        fields below are editable — everything else in the file is out of scope.
      </InlineBanner>

      {readOnly && (
        <InlineBanner tone="info" layout="inline" title="Read-only">
          You have read-only access to this share. You can review the values below but Save
          is disabled.
        </InlineBanner>
      )}

      <CadenceCard teamID={teamID} share={share} readOnly={readOnly} />

      <RecentRunsCard teamID={teamID} share={share} />

      {fields.length === 0 ? (
        <Card>
          <EmptyState
            title="Nothing to edit yet"
            message={`This ${
              share.category ? `"${share.category}" ` : ""
            }section isn't set up in the config file yet, so there are no fields to edit. Ask an administrator to add it.`}
          />
        </Card>
      ) : (
        fields.map((f) =>
          f.kind === "string" ? (
            <StringField
              key={f.path}
              field={f}
              value={draft[f.path] as string}
              disabled={readOnly}
              onChange={(v) => setField(f.path, v)}
            />
          ) : (
            <ArrayField
              key={f.path}
              field={f}
              value={draft[f.path] as string[]}
              disabled={readOnly}
              onChange={(v) => setField(f.path, v)}
            />
          ),
        )
      )}

      {fields.length > 0 && (
        <div className="sticky bottom-0 -mx-4 border-t border-border-subtle bg-surface-0/95 px-4 py-3 backdrop-blur sm:-mx-6 sm:px-6">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <StatusLine status={status} readOnly={readOnly} dirty={dirty} />
            <div className="flex items-center gap-2">
              <Button
                variant="ghost"
                size="sm"
                disabled={saving || !dirty || readOnly}
                onClick={() => {
                  setDraft(baseline);
                  setStatus({ kind: "idle" });
                }}
              >
                Reset
              </Button>
              <Button
                variant="primary"
                size="md"
                loading={saving}
                disabled={saving || !dirty || readOnly}
                onClick={() => void onSave()}
              >
                Save changes
              </Button>
            </div>
          </div>
        </div>
      )}

      {conflict && (
        <ConflictDialog
          fields={fields}
          yours={draft}
          server={conflict.serverDraft}
          onCancel={() => setConflict(null)}
          onOverwrite={() => void onSave(conflict.serverSha)}
          onAdoptServer={() => {
            setDraft(conflict.serverDraft);
            setBaseline(conflict.serverDraft);
            setSha(conflict.serverSha);
            setConflict(null);
            setStatus({ kind: "idle" });
          }}
        />
      )}
    </div>
  );
}

function StatusLine({
  status,
  readOnly,
  dirty,
}: {
  status: SaveStatus;
  readOnly: boolean;
  dirty: boolean;
}) {
  if (readOnly) return <span className="text-xs text-fg-subtle">Read-only share</span>;
  if (status.kind === "saved") {
    return (
      <span className="text-xs text-success-fg">
        Saved · {status.changed} field{status.changed === 1 ? "" : "s"} updated
      </span>
    );
  }
  if (status.kind === "error") {
    return <span className="text-xs text-danger-fg">{status.message}</span>;
  }
  return (
    <span className="text-xs text-fg-subtle">{dirty ? "Unsaved changes" : "No changes"}</span>
  );
}
