import { useCallback, useMemo, useState } from "react";
import {
  ChevronLeftIcon,
  FileTextIcon,
  ReloadIcon,
} from "@radix-ui/react-icons";

import { IconButton, Tooltip } from "@/components/ui";
import { useRunFiles } from "@/hooks/useRunFiles";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";
import type { RunFile, RunFileStatus } from "@/api/runs";

// Collapsed mirrors VSCode's activity bar (~36px); expanded matches the
// source-control panel's default. Drag-to-resize is deliberately omitted
// — collapse/expand covers the 90% case and keeps the panel predictable.
const COLLAPSED_PX = 36;
const EXPANDED_PX = 280;
const COLLAPSED_KEY = "run-console-v1.files-collapsed";

interface FilesPanelProps {
  runId: string;
  onSelectFile: (file: RunFile) => void;
}

export default function FilesPanel({ runId, onSelectFile }: FilesPanelProps) {
  const { data, loading, error, refresh } = useRunFiles(runId);
  const [collapsed, setCollapsed] = useState<boolean>(() =>
    readBooleanFlag(COLLAPSED_KEY, true),
  );
  const toggleCollapsed = useCallback(() => {
    setCollapsed((prev) => {
      const next = !prev;
      writeBooleanFlag(COLLAPSED_KEY, next);
      return next;
    });
  }, []);

  // Sort once per data change: untracked last, otherwise alphabetical.
  // The grouping mirrors VSCode's behaviour where new files cluster at
  // the bottom — easier to scan when the list is long.
  const sortedFiles = useMemo(() => {
    if (!data?.files) return [];
    return [...data.files].sort((a, b) => {
      const ua = a.status === "??" ? 1 : 0;
      const ub = b.status === "??" ? 1 : 0;
      if (ua !== ub) return ua - ub;
      return a.path.localeCompare(b.path);
    });
  }, [data?.files]);

  const fileCount = data?.files.length ?? 0;

  if (collapsed) {
    return (
      <aside
        style={{ width: COLLAPSED_PX }}
        className="flex flex-col items-center border-r border-border-default bg-surface-1 py-2 gap-2 shrink-0"
      >
        <Tooltip content={`Show modified files (${fileCount})`}>
          <button
            type="button"
            onClick={toggleCollapsed}
            aria-label="Show modified files"
            className="relative inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-muted hover:bg-surface-2 hover:text-fg-default"
          >
            <FileTextIcon />
            {fileCount > 0 && (
              <span className="absolute -top-1 -right-1 inline-flex h-3.5 min-w-3.5 items-center justify-center rounded-full bg-accent px-1 text-[9px] font-semibold leading-none text-fg-onAccent">
                {fileCount > 99 ? "99+" : fileCount}
              </span>
            )}
          </button>
        </Tooltip>
      </aside>
    );
  }

  return (
    <aside
      style={{ width: EXPANDED_PX }}
      className="flex flex-col border-r border-border-default bg-surface-1 shrink-0 min-h-0"
    >
      <header className="flex items-center gap-1 px-2 py-1.5 border-b border-border-default">
        <FileTextIcon className="text-fg-muted shrink-0" />
        <span className="text-xs font-semibold uppercase tracking-wide text-fg-default">
          Files
        </span>
        {fileCount > 0 && (
          <span className="ml-1 inline-flex items-center justify-center rounded-md bg-surface-2 px-1.5 text-[10px] font-medium text-fg-muted">
            {fileCount}
          </span>
        )}
        <div className="ml-auto flex items-center gap-1">
          <IconButton
            label="Refresh"
            size="sm"
            variant="ghost"
            onClick={refresh}
            disabled={loading}
          >
            <ReloadIcon className={loading ? "animate-spin" : undefined} />
          </IconButton>
          <IconButton
            label="Hide files panel"
            size="sm"
            variant="ghost"
            onClick={toggleCollapsed}
          >
            <ChevronLeftIcon />
          </IconButton>
        </div>
      </header>
      <div className="flex-1 min-h-0 overflow-y-auto">
        {error ? (
          <EmptyState message={error} />
        ) : !data ? (
          loading ? (
            <EmptyState message="Loading…" />
          ) : (
            <EmptyState message="" />
          )
        ) : !data.available ? (
          <EmptyState message={reasonLabel(data.reason)} />
        ) : sortedFiles.length === 0 ? (
          <EmptyState message="No changes" />
        ) : (
          <ul className="py-1">
            {sortedFiles.map((f) => (
              <FileRow key={f.path + f.status} file={f} onClick={onSelectFile} />
            ))}
          </ul>
        )}
      </div>
      {data?.work_dir && (
        <footer className="border-t border-border-default px-2 py-1 text-[10px] text-fg-subtle truncate">
          <Tooltip content={data.work_dir}>
            <span className="truncate block">
              {data.worktree ? "worktree: " : "cwd: "}
              {basename(data.work_dir)}
            </span>
          </Tooltip>
        </footer>
      )}
    </aside>
  );
}

interface FileRowProps {
  file: RunFile;
  onClick: (file: RunFile) => void;
}

function FileRow({ file, onClick }: FileRowProps) {
  const dir = dirname(file.path);
  const base = basename(file.path);
  const tooltip = file.old_path
    ? `${file.old_path} → ${file.path}`
    : file.path;
  return (
    <li>
      <Tooltip content={tooltip}>
        <button
          type="button"
          onClick={() => onClick(file)}
          className="flex w-full items-center gap-2 px-2 py-1 text-left hover:bg-surface-2 focus:bg-surface-2 focus:outline-none"
        >
          <StatusBadge status={file.status} />
          <span className="truncate text-xs text-fg-default">{base}</span>
          {dir && (
            <span className="ml-auto truncate pl-2 text-[10px] text-fg-subtle min-w-0">
              {dir}
            </span>
          )}
        </button>
      </Tooltip>
    </li>
  );
}

// VSCode-style colour palette: modified=yellow, added=green,
// deleted=red, untracked=green-muted, renamed=blue. The badge is a
// fixed 14px square so columns align even with multi-letter codes.
function StatusBadge({ status }: { status: RunFileStatus }) {
  const cls = STATUS_CLASS[status] ?? "text-fg-muted";
  const letter = status === "??" ? "U" : status;
  return (
    <span
      className={`inline-flex h-4 w-4 shrink-0 items-center justify-center rounded text-[10px] font-bold leading-none ${cls}`}
      aria-label={`status ${status}`}
    >
      {letter}
    </span>
  );
}

const STATUS_CLASS: Record<string, string> = {
  M: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
  A: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
  D: "bg-rose-500/15 text-rose-600 dark:text-rose-400",
  R: "bg-sky-500/15 text-sky-600 dark:text-sky-400",
  "??": "bg-emerald-500/10 text-emerald-700/80 dark:text-emerald-400/70",
};

function EmptyState({ message }: { message: string }) {
  return (
    <div className="flex h-full items-center justify-center px-3 py-8 text-center text-xs text-fg-subtle">
      {message}
    </div>
  );
}

function reasonLabel(reason: string | undefined): string {
  switch (reason) {
    case "no_workdir":
      return "No working directory recorded for this run";
    case "not_git_repo":
      return "Not a git repository";
    default:
      return reason ?? "Files unavailable";
  }
}

function basename(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? path : path.slice(i + 1);
}

function dirname(path: string): string {
  const i = path.lastIndexOf("/");
  return i < 0 ? "" : path.slice(0, i);
}

