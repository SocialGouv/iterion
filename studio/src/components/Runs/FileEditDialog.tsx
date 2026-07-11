import { useEffect, useState } from "react";
import Editor from "@monaco-editor/react";
import { useQueryClient } from "@tanstack/react-query";

import { Button, Dialog, Spinner } from "@/components/ui";
import {
  getRunFileContent,
  saveRunFileContent,
  type RunFileContent,
} from "@/api/runs";
import { useThemeStore } from "@/store/theme";
import { inferMonacoLanguage } from "@/lib/inferMonacoLanguage";

interface FileEditDialogProps {
  runId: string;
  // The worktree-relative path to edit, or null to close. A path that
  // doesn't exist yet opens a fresh empty buffer (e.g. creating .gitignore).
  path: string | null;
  onClose: () => void;
}

// FileEditDialog opens an EDITABLE Monaco buffer on one file in the run's
// live worktree. It is the read/write counterpart to FileDiffDialog (which
// stays read-only): the operator can fix a stray build/cache dir in
// .gitignore, or patch a file, without dropping to a terminal. Loaded on
// demand — the contents come from /files/content and writes go back through
// /files/content (PUT), both strictly scoped to the run's work_dir.
export default function FileEditDialog({
  runId,
  path,
  onClose,
}: FileEditDialogProps) {
  const [original, setOriginal] = useState<string>("");
  const [value, setValue] = useState<string>("");
  const [meta, setMeta] = useState<RunFileContent | null>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const resolvedTheme = useThemeStore((s) => s.resolved);
  const queryClient = useQueryClient();

  useEffect(() => {
    if (!path) {
      setOriginal("");
      setValue("");
      setMeta(null);
      setError(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    setMeta(null);
    getRunFileContent(runId, path)
      .then((res) => {
        if (cancelled) return;
        setMeta(res);
        const text = res.binary ? "" : res.content;
        setOriginal(text);
        setValue(text);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load file");
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [runId, path]);

  const open = path !== null;
  const language = path ? inferMonacoLanguage(path) : "plaintext";
  const monacoTheme = resolvedTheme === "dark" ? "vs-dark" : "vs";
  const dirty = value !== original;
  const binary = meta?.binary ?? false;
  const canSave = open && !loading && !saving && !binary && dirty;

  async function handleSave(next: string) {
    if (!path || saving || binary) return;
    setSaving(true);
    setError(null);
    try {
      await saveRunFileContent(runId, path, next);
      setOriginal(next);
      // Refresh the files tree / diff so the new state (and the large-
      // changeset count) reflects the edit immediately. Invalidate every
      // mode by matching the run-files key prefix.
      queryClient.invalidateQueries({ queryKey: ["run-files", runId] });
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Failed to save file");
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={path ?? "Edit file"}
      description={
        binary
          ? "Binary file — not editable"
          : meta && !meta.exists
            ? "New file — will be created on save"
            : "Editing the run worktree — saved directly to disk"
      }
      widthClass="max-w-[90vw] w-[90vw]"
      footer={
        <>
          {error && (
            <span className="mr-auto truncate text-xs text-danger">{error}</span>
          )}
          <Button variant="ghost" size="sm" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            size="sm"
            onClick={() => handleSave(value)}
            disabled={!canSave}
            loading={saving}
          >
            {dirty ? "Save" : "Saved"}
          </Button>
        </>
      }
    >
      <div className="h-[75vh] -mx-4 -my-3 flex flex-col">
        {error && !meta ? (
          <div className="flex flex-1 items-center justify-center px-4 text-sm text-danger">
            {error}
          </div>
        ) : loading || !meta ? (
          <div className="flex flex-1 items-center justify-center text-sm text-fg-subtle">
            <Spinner size="sm" label="Loading file" />
          </div>
        ) : binary ? (
          <div className="flex flex-1 items-center justify-center text-sm text-fg-subtle">
            Binary file — open it in an external editor.
          </div>
        ) : (
          <Editor
            theme={monacoTheme}
            language={language}
            value={value}
            onChange={(v) => setValue(v ?? "")}
            onMount={(editor, monaco) => {
              // Read the live buffer via editor.getValue() so the
              // imperatively-registered command never closes over a stale
              // `value` from the mount-time render.
              editor.addCommand(
                monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS,
                () => {
                  void handleSave(editor.getValue());
                },
              );
            }}
            options={{
              readOnly: false,
              automaticLayout: true,
              minimap: { enabled: false },
              scrollBeyondLastLine: false,
            }}
          />
        )}
      </div>
    </Dialog>
  );
}
