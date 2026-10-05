import { useEffect, useRef, useState } from "react";
import Editor from "@/lib/monaco";
import { useQueryClient } from "@tanstack/react-query";

import { Button, Dialog, Spinner } from "@/components/ui";
import {
  getRunFileContent,
  saveRunFileContent,
  type RunFileContent,
} from "@/api/runs";
import { useThemeStore } from "@/store/theme";
import { runFileBufferKey, useEditBuffersStore } from "@/store/editBuffers";
import { useConfirm } from "@/hooks/useConfirm";
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
//
// The typed buffer lives in `store/editBuffers.ts`, keyed by run and path
// (#1755) — not in this component. The dialog is mounted by RunView, so any
// route change out of the run — or the run view's own unmount — used to take
// the text with no prompt and no way back. Held in the store it outlives the
// unmount, is adopted back when the same file is opened again, and the
// app-wide unload warning sees it. What the dialog still owns is the ONE
// exit an author triggers from inside it — Cancel, Escape, an outside
// click — and that asks before dropping dirty text, the way the bundle
// drawer's own gate does.
export default function FileEditDialog({
  runId,
  path,
  onClose,
}: FileEditDialogProps) {
  const [meta, setMeta] = useState<RunFileContent | null>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const resolvedTheme = useThemeStore((s) => s.resolved);
  const queryClient = useQueryClient();
  const { confirm, dialog: confirmDialog } = useConfirm();

  const key = path ? runFileBufferKey(runId, path) : null;
  // The buffer itself, straight from the store: the text on screen and the
  // dirty check both read it, so a buffer adopted from an unmounted dialog
  // shows with its text and its dirtiness intact.
  const buffer = useEditBuffersStore((s) => (key ? s.runFiles[key] ?? null : null));
  const value = buffer?.value ?? "";
  const original = buffer?.original ?? "";
  const keyRef = useRef(key);
  keyRef.current = key;

  useEffect(() => {
    if (!path) {
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
        const store = useEditBuffersStore.getState();
        const bufKey = runFileBufferKey(runId, path);
        // A buffer held from an earlier mount of this same file is adopted,
        // not overwritten: the fetched text is what the file reads NOW, and
        // putting it in the buffer would mark the held text clean. The
        // author's diff stays against what they started from.
        if (!store.runFiles[bufKey]) {
          const text = res.binary ? "" : res.content;
          store.setRunFile(bufKey, { value: text, original: text });
        }
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

  // Leaving THIS file — another path, or the dialog unmounting with its run
  // view — releases a CLEAN buffer (it holds nothing) and keeps a dirty one,
  // which stays adopted-able and counted by the unload warning. The dirty
  // release happens only through the gate below or a successful save. The
  // key is taken from THIS effect instance's props, not a ref: the cleanup
  // of the old key runs after the render that already moved the ref.
  useEffect(() => {
    const bufKey = key;
    return () => {
      if (!bufKey) return;
      const store = useEditBuffersStore.getState();
      const held = store.runFiles[bufKey];
      if (held && held.value === held.original) store.setRunFile(bufKey, null);
    };
  }, [key]);

  const open = path !== null;
  const language = path ? inferMonacoLanguage(path) : "plaintext";
  const monacoTheme = resolvedTheme === "dark" ? "vs-dark" : "vs";
  const dirty = value !== original;
  const binary = meta?.binary ?? false;
  const canSave = open && !loading && !saving && !binary && dirty;

  async function handleSave(next: string) {
    if (!path || saving || binary) return;
    const bufKey = runFileBufferKey(runId, path);
    setSaving(true);
    setError(null);
    try {
      await saveRunFileContent(runId, path, next);
      const store = useEditBuffersStore.getState();
      const held = store.runFiles[bufKey];
      // Settled only into a buffer that is still there: one discarded while
      // the write was in flight is not resurrected by its answer. Text typed
      // during the flight stays, dirty against what landed.
      if (held) store.setRunFile(bufKey, { ...held, original: next });
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

  // The one close this dialog triggers from inside — Cancel, Escape, an
  // outside click. It asks before taking dirty text; a parent closing the
  // dialog (switching runs) releases nothing here and the buffer stays
  // held for the run it belongs to.
  const requestClose = async () => {
    const bufKey = keyRef.current;
    const held = bufKey ? useEditBuffersStore.getState().runFiles[bufKey] ?? null : null;
    if (held && held.value !== held.original) {
      const ok = await confirm({
        title: "Discard this text?",
        message:
          "You have not saved what you typed in this file. Closing the dialog drops it.",
        confirmLabel: "Discard",
        confirmVariant: "danger",
      });
      if (!ok) return;
    }
    if (bufKey) useEditBuffersStore.getState().setRunFile(bufKey, null);
    onClose();
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) void requestClose();
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
          <Button variant="ghost" size="sm" onClick={() => void requestClose()}>
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
            onChange={(v) => {
              const bufKey = keyRef.current;
              if (!bufKey) return;
              const store = useEditBuffersStore.getState();
              const held = store.runFiles[bufKey];
              // Typing before the read answered has no buffer to write to.
              store.setRunFile(bufKey, {
                value: v ?? "",
                original: held?.original ?? value,
              });
            }}
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
      {confirmDialog}
    </Dialog>
  );
}
