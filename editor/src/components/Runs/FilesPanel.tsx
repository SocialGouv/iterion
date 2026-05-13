import { useCallback, useEffect, useMemo, useState } from "react";

import {
  ChevronDownIcon,
  ChevronRightIcon,
  ReloadIcon,
} from "@radix-ui/react-icons";

import { IconButton, Tooltip } from "@/components/ui";
import { useRunFiles } from "@/hooks/useRunFiles";
import {
  buildFileTree,
  type TreeFile,
  type TreeFolder,
  type TreeNode,
} from "@/lib/fileTree";
import { basename } from "@/lib/format";
import type { RunFile, RunFileStatus, RunFilesMode } from "@/api/runs";

type ViewMode = "uncommitted" | "branch";

const MODE_STORAGE_KEY = "run-console-v1.files-mode";

// readPersistedMode pulls the user's last segmented-control selection
// from localStorage. Scoped globally (not per-runId) because the
// preference is about *how* the user reads diffs, not *which* run —
// across runs we want the same view.
function readPersistedMode(): ViewMode {
  if (typeof window === "undefined") return "uncommitted";
  const raw = window.localStorage.getItem(MODE_STORAGE_KEY);
  if (raw === "branch") return "branch";
  return "uncommitted";
}

function writePersistedMode(mode: ViewMode) {
  if (typeof window === "undefined") return;
  window.localStorage.setItem(MODE_STORAGE_KEY, mode);
}

interface FilesPanelProps {
  runId: string;
  // onSelectFile carries the current mode so FileDiffDialog can request
  // the right range from the backend (uncommitted vs branch).
  onSelectFile: (file: RunFile, mode: RunFilesMode) => void;
}

// FilesPanel renders the modified-files tree — wrapped by LeftPanel
// which owns the collapse state, the panel chrome, and the tab strip.
// The list is presented as a Git-Graph-style tree: folders compact
// single-child chains into breadcrumb labels (".github / workflows"),
// each file shows "+N | -N" line counts in green/red, and folders show
// the same aggregate when they are collapsed (so a folded directory
// still tells you how much churn lives below).
export default function FilesPanel({ runId, onSelectFile }: FilesPanelProps) {
  const [mode, setMode] = useState<ViewMode>(() => readPersistedMode());
  // Persist mode changes (writePersistedMode is a no-op on SSR; the
  // initializer above handles the cold-load case).
  useEffect(() => {
    writePersistedMode(mode);
  }, [mode]);

  const { data, loading, error, refresh } = useRunFiles(runId, mode);

  // When the backend signals worktree_gone (uncommitted requested but
  // worktree was torn down), fall back to branch view automatically so
  // the user sees something rather than an empty placeholder. Only
  // applies on data refresh, so manual toggles still work in any state.
  useEffect(() => {
    if (data?.reason === "worktree_gone" && mode === "uncommitted") {
      setMode("branch");
    }
  }, [data?.reason, mode]);

  const tree = useMemo(() => buildFileTree(data?.files ?? []), [data?.files]);
  const fileCount = data?.files.length ?? 0;
  // Disable the uncommitted segment when no worktree exists — the
  // backend can't compute it. Same condition the auto-fallback above
  // keys off, surfaced visually so the user understands the constraint.
  const uncommittedDisabled = data?.reason === "worktree_gone";

  // Default state: every folder expanded. Set tracks pathKeys the user
  // has *explicitly* collapsed — this way new files arriving live (the
  // panel auto-refreshes on node_finished events) appear under
  // already-expanded ancestors instead of being hidden.
  const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set());
  const toggle = useCallback((pathKey: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(pathKey)) {
        next.delete(pathKey);
      } else {
        next.add(pathKey);
      }
      return next;
    });
  }, []);

  const handleSelectFile = useCallback(
    (file: RunFile) => {
      onSelectFile(file, mode);
    },
    [onSelectFile, mode],
  );

  return (
    <div className="flex flex-col min-h-0 min-w-0 flex-1 w-full">
      <header className="flex flex-col gap-1 border-b border-border-default px-2 py-1">
        <div className="flex items-center gap-1">
          <ModeSegmented
            mode={mode}
            onChange={setMode}
            uncommittedDisabled={uncommittedDisabled}
            count={fileCount}
          />
          <div className="ml-auto">
            <IconButton
              label="Refresh"
              size="sm"
              variant="ghost"
              onClick={refresh}
              disabled={loading}
            >
              <ReloadIcon className={loading ? "animate-spin" : undefined} />
            </IconButton>
          </div>
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
        ) : tree.length === 0 ? (
          <EmptyState message={emptyMessage(mode)} />
        ) : (
          <div className="py-1 text-xs">
            {tree.map((node) => (
              <TreeRow
                key={node.pathKey}
                node={node}
                collapsed={collapsed}
                onToggle={toggle}
                onSelectFile={handleSelectFile}
              />
            ))}
          </div>
        )}
      </div>
      {data?.work_dir && (
        <footer className="border-t border-border-default px-2 py-1 text-[10px] text-fg-subtle truncate">
          <Tooltip content={footerTooltip(data, mode)}>
            <span className="truncate block">{footerLabel(data, mode)}</span>
          </Tooltip>
        </footer>
      )}
    </div>
  );
}

interface ModeSegmentedProps {
  mode: ViewMode;
  onChange: (mode: ViewMode) => void;
  uncommittedDisabled: boolean;
  count: number;
}

// ModeSegmented is the two-pill segmented control above the tree.
// "Uncommitted" maps to git status, "Branch" to BaseCommit..HEAD.
// The count badge tracks the currently-visible mode so the active pill
// stays consistent with the tree below.
function ModeSegmented({
  mode,
  onChange,
  uncommittedDisabled,
  count,
}: ModeSegmentedProps) {
  return (
    <div className="inline-flex overflow-hidden rounded-md border border-border-default text-[10px]">
      <SegmentButton
        active={mode === "uncommitted"}
        disabled={uncommittedDisabled}
        onClick={() => onChange("uncommitted")}
        label="Uncommitted"
        tooltip={
          uncommittedDisabled
            ? "Worktree no longer available — only the branch view is reachable."
            : "Files modified but not yet committed (git status)."
        }
        count={mode === "uncommitted" ? count : undefined}
      />
      <SegmentButton
        active={mode === "branch"}
        onClick={() => onChange("branch")}
        label="Branch"
        tooltip="All changes this run introduced, vs. the source branch (base..HEAD)."
        count={mode === "branch" ? count : undefined}
      />
    </div>
  );
}

function SegmentButton({
  active,
  disabled,
  onClick,
  label,
  tooltip,
  count,
}: {
  active: boolean;
  disabled?: boolean;
  onClick: () => void;
  label: string;
  tooltip: string;
  count?: number;
}) {
  const cls = active
    ? "bg-surface-2 text-fg-default"
    : "bg-transparent text-fg-muted hover:bg-surface-2";
  return (
    <Tooltip content={tooltip}>
      <button
        type="button"
        onClick={onClick}
        disabled={disabled}
        className={`px-2 py-0.5 disabled:opacity-50 disabled:cursor-not-allowed focus:outline-none ${cls}`}
      >
        <span>{label}</span>
        {typeof count === "number" && (
          <span className="ml-1 inline-flex items-center justify-center rounded-md bg-surface-3 px-1 text-[9px] font-medium text-fg-muted">
            {count}
          </span>
        )}
      </button>
    </Tooltip>
  );
}

interface TreeRowProps {
  node: TreeNode;
  collapsed: Set<string>;
  onToggle: (pathKey: string) => void;
  onSelectFile: (file: RunFile) => void;
}

function TreeRow({ node, collapsed, onToggle, onSelectFile }: TreeRowProps) {
  if (node.kind === "folder") {
    const isCollapsed = collapsed.has(node.pathKey);
    return (
      <>
        <FolderRow
          folder={node}
          collapsed={isCollapsed}
          onToggle={() => onToggle(node.pathKey)}
        />
        {!isCollapsed &&
          node.children.map((child) => (
            <TreeRow
              key={child.pathKey}
              node={child}
              collapsed={collapsed}
              onToggle={onToggle}
              onSelectFile={onSelectFile}
            />
          ))}
      </>
    );
  }
  return <FileRow node={node} onSelectFile={onSelectFile} />;
}

// Pixel offsets for indentation. We use inline styles instead of
// Tailwind utility classes because Tailwind's `pl-N` doesn't compose
// for arbitrary depth — each level needs a class that JIT can pick up
// at build time, which doesn't work with a runtime-driven number.
const INDENT_BASE = 8; // left padding of the outermost row
const INDENT_PER_LEVEL = 12; // matches VS Code tree density
const CHEVRON_SLOT = 14; // h-3 chevron + gap reserved for files

function FolderRow({
  folder,
  collapsed,
  onToggle,
}: {
  folder: TreeFolder;
  collapsed: boolean;
  onToggle: () => void;
}) {
  // Aggregates show only when collapsed: an expanded folder's children
  // already render their own counts, so summing them again on the
  // header is just visual noise.
  const showAggregate =
    collapsed && (folder.added > 0 || folder.deleted > 0);
  return (
    <button
      type="button"
      onClick={onToggle}
      style={{ paddingLeft: INDENT_BASE + folder.depth * INDENT_PER_LEVEL }}
      className="flex w-full items-center gap-1 py-0.5 pr-2 text-left hover:bg-surface-2 focus:bg-surface-2 focus:outline-none"
    >
      {collapsed ? (
        <ChevronRightIcon className="h-3 w-3 shrink-0 text-fg-subtle" />
      ) : (
        <ChevronDownIcon className="h-3 w-3 shrink-0 text-fg-subtle" />
      )}
      <span className="truncate text-fg-muted">{folder.label}</span>
      {showAggregate && (
        <span className="ml-auto shrink-0 pl-2 text-[10px] tabular-nums">
          {folder.added > 0 && (
            <span className="text-emerald-500">+{folder.added}</span>
          )}
          {folder.added > 0 && folder.deleted > 0 && (
            <span className="text-fg-subtle"> </span>
          )}
          {folder.deleted > 0 && (
            <span className="text-rose-500">-{folder.deleted}</span>
          )}
        </span>
      )}
    </button>
  );
}

function FileRow({
  node,
  onSelectFile,
}: {
  node: TreeFile;
  onSelectFile: (file: RunFile) => void;
}) {
  const f = node.file;
  const tooltip = f.old_path ? `${f.old_path} → ${f.path}` : f.path;
  return (
    <Tooltip content={tooltip}>
      <button
        type="button"
        onClick={() => onSelectFile(f)}
        style={{
          paddingLeft:
            INDENT_BASE + node.depth * INDENT_PER_LEVEL + CHEVRON_SLOT,
        }}
        className="flex w-full items-center gap-2 py-0.5 pr-2 text-left hover:bg-surface-2 focus:bg-surface-2 focus:outline-none"
      >
        <StatusDot status={f.status} />
        <span className="truncate min-w-0 text-fg-default">{node.label}</span>
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
  );
}

// VSCode-style palette. Compared to the previous flat-list `StatusBadge`,
// the dot is leaner (no filled background) so the row chrome doesn't
// compete with the line-count column on the right. The letter form is
// kept rather than a shape so screen readers can announce status
// without aria gymnastics.
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
    case "no_baseline":
      return "This run has no base commit — branch diff unavailable";
    case "worktree_gone":
      return "Worktree was cleaned up — switch to Branch view";
    default:
      return reason ?? "Files unavailable";
  }
}

function emptyMessage(mode: ViewMode): string {
  return mode === "branch" ? "No committed changes yet" : "No changes";
}

// isLive defaults to true when the field is absent so pre-feature
// backends keep their original wording (badges interpreted as
// uncommitted state).
function isLive(data: { live?: boolean }): boolean {
  return data.live !== false;
}

// footerLabel disambiguates the visible mode + lifecycle:
// - uncommitted on a live worktree → "Working tree (worktree): name"
// - branch on a live worktree → "Branch vs. source: BaseCommit..HEAD"
// - branch on a finalized run → "Committed in this run"
function footerLabel(
  data: {
    worktree?: boolean;
    live?: boolean;
    work_dir?: string;
  },
  mode: ViewMode,
): string {
  if (mode === "branch") {
    if (isLive(data)) {
      const where = data.worktree ? "worktree" : "cwd";
      return `Branch vs. source · ${where}: ${basename(data.work_dir ?? "")}`;
    }
    return "Committed in this run";
  }
  const where = data.worktree ? "worktree" : "cwd";
  return `Working tree (${where}): ${basename(data.work_dir ?? "")}`;
}

function footerTooltip(
  data: {
    worktree?: boolean;
    live?: boolean;
    work_dir?: string;
  },
  mode: ViewMode,
): string {
  if (mode === "branch") {
    if (isLive(data)) {
      return `All changes this run has committed so far, vs. the source branch (BaseCommit..HEAD). Worktree: ${
        data.work_dir ?? "<unknown>"
      }`;
    }
    return `Files committed by this run, diffed against the run's base commit. The worktree at ${
      data.work_dir ?? "<unknown>"
    } has been cleaned up; the storage branch is the source of truth.`;
  }
  return data.work_dir ?? "";
}

