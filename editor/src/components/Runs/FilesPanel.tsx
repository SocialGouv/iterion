import { useCallback, useMemo, useState } from "react";

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
import type { RunFile, RunFileStatus } from "@/api/runs";

interface FilesPanelProps {
  runId: string;
  onSelectFile: (file: RunFile) => void;
}

// FilesPanel renders the modified-files tree — wrapped by LeftPanel
// which owns the collapse state, the panel chrome, and the tab strip.
// The list is presented as a Git-Graph-style tree: folders compact
// single-child chains into breadcrumb labels (".github / workflows"),
// each file shows "+N | -N" line counts in green/red, and folders show
// the same aggregate when they are collapsed (so a folded directory
// still tells you how much churn lives below).
export default function FilesPanel({ runId, onSelectFile }: FilesPanelProps) {
  const { data, loading, error, refresh } = useRunFiles(runId);

  const tree = useMemo(() => buildFileTree(data?.files ?? []), [data?.files]);
  const fileCount = data?.files.length ?? 0;

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

  return (
    <div className="flex flex-col min-h-0 min-w-0 flex-1 w-full">
      <header className="flex items-center gap-1 px-2 py-1 border-b border-border-default">
        {fileCount > 0 && (
          <span className="inline-flex items-center justify-center rounded-md bg-surface-2 px-1.5 text-[10px] font-medium text-fg-muted">
            {fileCount}
          </span>
        )}
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
          <EmptyState message="No changes" />
        ) : (
          <div className="py-1 text-xs">
            {tree.map((node) => (
              <TreeRow
                key={node.pathKey}
                node={node}
                collapsed={collapsed}
                onToggle={toggle}
                onSelectFile={onSelectFile}
              />
            ))}
          </div>
        )}
      </div>
      {data?.work_dir && (
        <footer className="border-t border-border-default px-2 py-1 text-[10px] text-fg-subtle truncate">
          <Tooltip content={footerTooltip(data)}>
            <span className="truncate block">{footerLabel(data)}</span>
          </Tooltip>
        </footer>
      )}
    </div>
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
    default:
      return reason ?? "Files unavailable";
  }
}

// isLive defaults to true when the field is absent so pre-feature
// backends keep their original wording (badges interpreted as
// uncommitted state).
function isLive(data: { live?: boolean }): boolean {
  return data.live !== false;
}

// footerLabel disambiguates the two source-of-truth modes:
// - live=true → files come from `git status` against a still-existing
//   worktree/cwd. Badges are uncommitted state.
// - live=false → files come from `git diff base..final` against the
//   storage branch (worktree gc'd). Badges are committed-in-this-run.
function footerLabel(data: {
  worktree?: boolean;
  live?: boolean;
  work_dir?: string;
}): string {
  if (!isLive(data)) {
    return "Committed in this run";
  }
  const where = data.worktree ? "worktree" : "cwd";
  return `Working tree (${where}): ${basename(data.work_dir ?? "")}`;
}

function footerTooltip(data: {
  worktree?: boolean;
  live?: boolean;
  work_dir?: string;
}): string {
  if (isLive(data)) {
    return data.work_dir ?? "";
  }
  return `Files committed by this run, diffed against the run's base commit. The worktree at ${
    data.work_dir ?? "<unknown>"
  } has been cleaned up; the storage branch is the source of truth.`;
}

