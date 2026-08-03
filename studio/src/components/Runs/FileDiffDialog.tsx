import { useQuery } from "@tanstack/react-query";
import { DiffEditor } from "@monaco-editor/react";

import { Button, Dialog } from "@/components/ui";
import {
  getReviewFileDiff,
  getRunFileDiff,
  type RunFile,
  type RunFilesMode,
} from "@/api/runs";
import { useThemeStore } from "@/store/theme";
import { errorMessage } from "@/lib/errorHints";
import { inferMonacoLanguage } from "@/lib/inferMonacoLanguage";

interface FileDiffDialogProps {
  runId: string;
  file: RunFile | null;
  // Forwarded to /files/diff so the backend picks the same range used
  // by the listing (uncommitted vs branch). Omitted → backend default.
  mode?: RunFilesMode;
  // When set, the diff is taken over that review gate's range instead of
  // the run's. A reviewer must see the file as they are being asked to
  // approve it — not as it stands after later nodes touched it.
  gate?: number;
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
  gate,
  onClose,
  onEdit,
}: FileDiffDialogProps) {
  const resolvedTheme = useThemeStore((s) => s.resolved);
  const path = file?.path ?? null;

  const diffQuery = useQuery({
    queryKey: ["run-file-diff", runId, path, mode ?? "", gate ?? -1],
    queryFn: () =>
      gate === undefined
        ? getRunFileDiff(runId, path!, { mode })
        : getReviewFileDiff(runId, path!, { gate }),
    enabled: !!path,
  });
  const diff = path ? diffQuery.data ?? null : null;
  const error = path && diffQuery.error ? errorMessage(diffQuery.error) : null;
  const loading = diffQuery.isLoading;

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
