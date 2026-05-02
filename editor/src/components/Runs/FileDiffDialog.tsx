import { useEffect, useState } from "react";
import { DiffEditor } from "@monaco-editor/react";

import { Dialog } from "@/components/ui";
import { getRunFileDiff, type RunFile, type RunFileDiff } from "@/api/runs";
import { useThemeStore } from "@/store/theme";
import { inferMonacoLanguage } from "@/lib/inferMonacoLanguage";

interface FileDiffDialogProps {
  runId: string;
  file: RunFile | null;
  onClose: () => void;
}

// FileDiffDialog opens Monaco's DiffEditor on the run's working
// directory. Loaded on demand because diffs can be megabytes and users
// typically only inspect a handful per run.
export default function FileDiffDialog({
  runId,
  file,
  onClose,
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
    getRunFileDiff(runId, path)
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
  }, [runId, path]);

  const open = file !== null;
  const language = path ? inferMonacoLanguage(path) : "plaintext";
  const monacoTheme = resolvedTheme === "dark" ? "vs-dark" : "vs";

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={path ?? "Diff"}
      description={file ? statusLabel(file.status) : undefined}
      widthClass="max-w-[90vw] w-[90vw]"
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
