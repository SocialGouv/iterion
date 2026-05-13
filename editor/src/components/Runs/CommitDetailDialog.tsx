import { useEffect, useMemo, useState } from "react";
import { DiffEditor } from "@monaco-editor/react";

import { Dialog, Tooltip } from "@/components/ui";
import {
  getRunCommit,
  getRunCommitFileDiff,
  type RunCommit,
  type RunCommitDetail,
  type RunFile,
  type RunFileDiff,
  type RunFileStatus,
} from "@/api/runs";
import { useThemeStore } from "@/store/theme";
import { inferMonacoLanguage } from "@/lib/inferMonacoLanguage";
import { formatRelative } from "@/lib/format";

interface CommitDetailDialogProps {
  runId: string;
  commit: RunCommit | null;
  onClose: () => void;
}

// CommitDetailDialog opens a GitHub-commit-page-style modal: header
// with SHA + subject + author + date, a left-column file list, and a
// Monaco DiffEditor for the currently-selected file. Each file shows
// its diff against the commit's parent (or the empty tree for root
// commits — DiffOfCommit handles that on the backend).
export default function CommitDetailDialog({
  runId,
  commit,
  onClose,
}: CommitDetailDialogProps) {
  const [detail, setDetail] = useState<RunCommitDetail | null>(null);
  const [loadingDetail, setLoadingDetail] = useState(false);
  const [detailError, setDetailError] = useState<string | null>(null);
  const [selectedPath, setSelectedPath] = useState<string | null>(null);

  const sha = commit?.sha ?? null;

  useEffect(() => {
    if (!sha) {
      setDetail(null);
      setDetailError(null);
      setSelectedPath(null);
      return;
    }
    let cancelled = false;
    setLoadingDetail(true);
    setDetailError(null);
    setDetail(null);
    setSelectedPath(null);
    getRunCommit(runId, sha)
      .then((res) => {
        if (cancelled) return;
        setDetail(res);
        // Preselect the first file so the diff area is never empty
        // when the modal opens.
        const first = res.files?.[0]?.path ?? null;
        setSelectedPath(first);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setDetailError(
          err instanceof Error ? err.message : "Failed to load commit",
        );
      })
      .finally(() => {
        if (cancelled) return;
        setLoadingDetail(false);
      });
    return () => {
      cancelled = true;
    };
  }, [runId, sha]);

  const open = commit !== null;
  const relativeDate = useMemo(
    () => (commit?.date ? formatRelative(commit.date) : ""),
    [commit?.date],
  );

  const subjectLabel = commit?.subject ?? sha ?? "Commit";
  const description = commit
    ? `${commit.short} · ${commit.author}${
        relativeDate ? ` · ${relativeDate}` : ""
      }`
    : undefined;

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={subjectLabel}
      description={description}
      widthClass="max-w-[90vw] w-[90vw]"
    >
      <div className="h-[75vh] -mx-4 -my-3 flex">
        {detailError ? (
          <div className="flex-1 flex items-center justify-center text-sm text-danger px-4">
            {detailError}
          </div>
        ) : !detail && loadingDetail ? (
          <div className="flex-1 flex items-center justify-center text-sm text-fg-subtle">
            Loading commit…
          </div>
        ) : detail && !detail.available ? (
          <div className="flex-1 flex items-center justify-center text-sm text-fg-subtle px-4">
            {detailReasonLabel(detail.reason)}
          </div>
        ) : detail && detail.files.length === 0 ? (
          <div className="flex-1 flex items-center justify-center text-sm text-fg-subtle">
            This commit has no file changes.
          </div>
        ) : detail ? (
          <>
            <aside className="w-72 shrink-0 border-r border-border-default overflow-y-auto">
              <FileList
                files={detail.files}
                selectedPath={selectedPath}
                onSelect={setSelectedPath}
              />
            </aside>
            <div className="flex-1 min-w-0">
              {selectedPath && commit && (
                <CommitFileDiff
                  runId={runId}
                  sha={commit.sha}
                  path={selectedPath}
                />
              )}
            </div>
          </>
        ) : null}
      </div>
    </Dialog>
  );
}

interface FileListProps {
  files: RunFile[];
  selectedPath: string | null;
  onSelect: (path: string) => void;
}

function FileList({ files, selectedPath, onSelect }: FileListProps) {
  return (
    <ul className="py-1 text-xs">
      {files.map((f) => {
        const active = f.path === selectedPath;
        const tooltip = f.old_path ? `${f.old_path} → ${f.path}` : f.path;
        return (
          <li key={f.path}>
            <Tooltip content={tooltip}>
              <button
                type="button"
                onClick={() => onSelect(f.path)}
                className={`flex w-full items-center gap-2 px-2 py-1 text-left focus:outline-none ${
                  active
                    ? "bg-surface-2 text-fg-default"
                    : "text-fg-default hover:bg-surface-2"
                }`}
              >
                <StatusDot status={f.status} />
                <span className="truncate min-w-0">{f.path}</span>
                <span className="ml-auto shrink-0 pl-2 text-[10px] tabular-nums">
                  {f.binary ? (
                    <span className="text-fg-subtle">(binary)</span>
                  ) : (
                    <>
                      <span className="text-emerald-500">+{f.added}</span>
                      <span className="text-fg-subtle"> | </span>
                      <span className="text-rose-500">-{f.deleted}</span>
                    </>
                  )}
                </span>
              </button>
            </Tooltip>
          </li>
        );
      })}
    </ul>
  );
}

interface CommitFileDiffProps {
  runId: string;
  sha: string;
  path: string;
}

function CommitFileDiff({ runId, sha, path }: CommitFileDiffProps) {
  const [diff, setDiff] = useState<RunFileDiff | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const resolvedTheme = useThemeStore((s) => s.resolved);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    setDiff(null);
    getRunCommitFileDiff(runId, sha, path)
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
  }, [runId, sha, path]);

  const language = inferMonacoLanguage(path);
  const monacoTheme = resolvedTheme === "dark" ? "vs-dark" : "vs";

  if (error) {
    return (
      <div className="h-full flex items-center justify-center text-sm text-danger px-4">
        {error}
      </div>
    );
  }
  if (loading || !diff) {
    return (
      <div className="h-full flex items-center justify-center text-sm text-fg-subtle">
        Loading diff…
      </div>
    );
  }
  if (diff.binary) {
    return (
      <div className="h-full flex items-center justify-center text-sm text-fg-subtle">
        Binary file not shown
      </div>
    );
  }
  return (
    <DiffEditor
      theme={monacoTheme}
      language={language}
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
  );
}

const STATUS_CLASS: Record<string, string> = {
  M: "text-amber-500",
  A: "text-emerald-500",
  D: "text-rose-500",
  R: "text-sky-500",
  "??": "text-emerald-400/70",
};

function StatusDot({ status }: { status: RunFileStatus }) {
  const cls = STATUS_CLASS[status] ?? "text-fg-muted";
  const letter = status === "??" ? "U" : status;
  return (
    <span
      className={`inline-flex h-3 w-3 shrink-0 items-center justify-center text-[10px] font-bold leading-none ${cls}`}
      aria-label={`status ${status}`}
    >
      {letter}
    </span>
  );
}

function detailReasonLabel(reason: string | undefined): string {
  switch (reason) {
    case "not_in_range":
      return "This commit is not part of this run.";
    default:
      return reason ?? "Commit unavailable";
  }
}
