import { useEffect, useState } from "react";
import { DiffEditor } from "@monaco-editor/react";

import { Button, Dialog } from "@/components/ui";
import {
  getRunFileDiff,
  type RunFile,
  type RunFileDiff,
  type RunFilesMode,
} from "@/api/runs";
import { useThemeStore } from "@/store/theme";
import { inferMonacoLanguage } from "@/lib/inferMonacoLanguage";

interface FileDiffDialogProps {
  runId: string;
  file: RunFile | null;
  // Forwarded to /files/diff so the backend picks the same range used
  // by the listing (uncommitted vs branch). Omitted → backend default.
  mode?: RunFilesMode;
  onClose: () => void;
  // When provided, renders an "Edit" affordance that switches from this
  // read-only diff to the editable FileEditDialog for the same path. The
  // diff stays read-only; editing is always a deliberate switch.
  onEdit?: (path: string) => void;
}

// FileDiffDialog opens Monaco's DiffEditor on the run's working
// directory. Loaded on demand because diffs can be megabytes and users
// typically only inspect a handful per run.
export default function FileDiffDialog({
  runId,
  file,
  mode,
  onClose,
  onEdit,
}: FileDiffDialogProps) {
  const [diff, setDiff] = useState<RunFileDiff | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const resolvedTheme = useThemeStore((s) => s.resolved);
  const path = file?.path ?? null;

  useEffect(() => {
    if (!path) {
      setDiff(null);
      setError(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    setDiff(null);
    getRunFileDiff(runId, path, { mode })
      .then((res) => {
        if (cancelled) return;
        setDiff(res);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : "Failed to load diff");
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [runId, path, mode]);

  const open = file !== null;
  const language = path ? inferMonacoLanguage(path) : "plaintext";
  const monacoTheme = resolvedTheme === "dark" ? "vs-dark" : "vs";
  // Offer "Edit" only for a real, non-binary path. A deleted file (status D)
  // has no working-tree content to edit, so it's excluded.
  const canEdit =
    Boolean(onEdit) && path !== null && !diff?.binary && file?.status !== "D";

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={path ?? "Diff"}
      description={file ? statusLabel(file.status) : undefined}
      widthClass="max-w-[90vw] w-[90vw]"
      footer={
        canEdit ? (
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              if (path && onEdit) onEdit(path);
            }}
          >
            Edit file
          </Button>
        ) : undefined
      }
    >
      <div className="h-[75vh] -mx-4 -my-3 flex flex-col">
        {error ? (
          <div className="flex-1 flex items-center justify-center text-sm text-danger px-4">
            {error}
          </div>
        ) : loading || !diff ? (
          <div className="flex-1 flex items-center justify-center text-sm text-fg-subtle">
            Loading diff…
          </div>
        ) : diff.binary ? (
          <div className="flex-1 flex items-center justify-center text-sm text-fg-subtle">
            Binary file not shown
          </div>
        ) : diff.oversized ? (
          <div className="flex-1 flex items-center justify-center text-sm text-fg-subtle">
            File too large to display
          </div>
        ) : (
          <DiffEditor
            theme={monacoTheme}
            language={language}
            // null/undefined contents map to empty string so Monaco
            // shows the missing side as a blank pane (correct visual
            // for added/deleted files).
            original={diff.before ?? ""}
            modified={diff.after ?? ""}
            options={{
              readOnly: true,
              renderSideBySide: true,
              ignoreTrimWhitespace: false,
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

function statusLabel(status: string): string {
  switch (status) {
    case "M":
      return "Modified";
    case "A":
      return "Added";
    case "D":
      return "Deleted";
    case "R":
      return "Renamed";
    case "??":
      return "Untracked";
    default:
      return status;
  }
}
