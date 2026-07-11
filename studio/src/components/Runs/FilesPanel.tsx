import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import {
  ChevronDownIcon,
  ChevronRightIcon,
  ReloadIcon,
} from "@radix-ui/react-icons";

import { EmptyState, IconButton, Spinner, Tooltip } from "@/components/ui";
import { useRunFiles } from "@/hooks/useRunFiles";
import { useToggleSet } from "@/hooks/useToggleSet";
import {
  buildFileTree,
  type TreeFile,
  type TreeFolder,
  type TreeNode,
} from "@/lib/fileTree";
import { basename } from "@/lib/format";
import type { RunFile, RunFilesMode } from "@/api/runs";
import { StatusDot } from "./StatusDot";

// View scope for the files tree. "combined" is the union of branch +
// uncommitted (each file tagged with a lifecycle), surfaced to the operator
// as "All changes"; it is the default for every run. (When the worktree has
// been cleaned up it can't be computed, so the panel falls back to "branch"
// — see the worktree_gone effect below.)
type ViewMode = "uncommitted" | "branch" | "combined";

// Above this many changed files we DEFAULT-COLLAPSE every folder on first
// load so the initial paint is a handful of folder rows rather than
// thousands of (tooltip-wrapped) file rows — which is what freezes the
// browser (a 20k+ un-gitignored cache crashed the UI; a committed Go
// vendor/ is legitimately thousands too). We never hide the files — they
// stay one expand away — and a non-blocking banner points at .gitignore
// when the count looks like a stray build/cache dir.
const LARGE_CHANGESET = 2000;

// Shown on the All changes + Uncommitted pills once the worktree is gone:
// both need a worktree to read pending changes, so only Branch stays
// reachable.
const WORKTREE_GONE_TIP =
  "Worktree no longer available — only the branch view is reachable.";

interface FilesPanelProps {
  runId: string;
  // onSelectFile carries the current mode so FileDiffDialog can request
  // the right range from the backend (uncommitted vs branch). In combined
  // mode the row forwards the per-file scope derived from its lifecycle.
  onSelectFile: (file: RunFile, mode: RunFilesMode) => void;
  // onEditFile opens a worktree file in an editable Monaco tab. Wired to the
  // large-changeset banner's "Edit .gitignore" shortcut so the operator can
  // gitignore a stray build/cache dir inline. Optional: the panel degrades
  // to the informational-only banner when absent.
  onEditFile?: (path: string) => void;
}

// FilesPanel renders the modified-files tree — wrapped by LeftPanel
// which owns the collapse state, the panel chrome, and the tab strip.
// The list is presented as a Git-Graph-style tree: folders compact
// single-child chains into breadcrumb labels (".github / workflows"),
// each file shows "+N | -N" line counts in green/red, and folders show
// the same aggregate when they are collapsed (so a folded directory
// still tells you how much churn lives below).
export default function FilesPanel({
  runId,
  onSelectFile,
  onEditFile,
}: FilesPanelProps) {
  // userMode is the operator's explicit segmented-control selection; null
  // means "follow the default" — which is always "combined" (All changes),
  // the superset view. Keeping the override here (rather than persisting it
  // globally to localStorage as we used to) lets each run open on All changes
  // while a manual pick stays put for the rest of the session.
  const [userMode, setUserMode] = useState<ViewMode | null>(null);
  // Reset the override when the run changes so each run opens at its own
  // phase-appropriate default rather than inheriting the previous run's pick.
  useEffect(() => {
    setUserMode(null);
  }, [runId]);
  const mode: ViewMode = userMode ?? "combined";

  const { data, loading, error, refresh } = useRunFiles(runId, mode);

  // When the backend signals worktree_gone (uncommitted/combined requested
  // but the worktree was torn down), fall back to branch view automatically
  // so the user sees something rather than an empty placeholder. Pins the
  // override to branch; manual toggles still work in any state.
  useEffect(() => {
    if (
      data?.reason === "worktree_gone" &&
      (mode === "uncommitted" || mode === "combined")
    ) {
      setUserMode("branch");
    }
  }, [data?.reason, mode]);

  const tree = useMemo(() => buildFileTree(data?.files ?? []), [data?.files]);
  const fileCount = data?.files.length ?? 0;
  const largeChangeset = fileCount > LARGE_CHANGESET;
  // Disable the uncommitted + combined segments when no worktree exists —
  // the backend can't read pending changes. Same condition the auto-fallback
  // above keys off, surfaced visually so the user understands the constraint.
  const worktreeGone = data?.reason === "worktree_gone";

  // Default state: every folder expanded. Set tracks pathKeys the user
  // has *explicitly* collapsed — this way new files arriving live (the
  // panel auto-refreshes on node_finished events) appear under
  // already-expanded ancestors instead of being hidden.
  const { set: collapsed, toggle, replace: replaceCollapsed } = useToggleSet<string>();

  // On a LARGE changeset, auto-collapse every folder once per (run, mode)
  // so the first paint is bounded (top-level folders only) instead of
  // mounting thousands of rows. A signature ref makes this fire once and
  // never fight the user's subsequent expand/collapse.
  const autoCollapsedSig = useRef<string | null>(null);
  useEffect(() => {
    const sig = `${runId}:${mode}`;
    if (largeChangeset && tree.length > 0 && autoCollapsedSig.current !== sig) {
      autoCollapsedSig.current = sig;
      replaceCollapsed(collectFolderKeys(tree));
    } else if (!largeChangeset) {
      autoCollapsedSig.current = null;
    }
  }, [largeChangeset, tree, runId, mode, replaceCollapsed]);

  const handleSelectFile = useCallback(
    (file: RunFile) => {
      // In combined mode each row forwards the scope matching its lifecycle
      // so the diff dialog reuses the existing two backend ranges: a
      // committed file diffs the branch range (base..HEAD), an uncommitted
      // file diffs the working tree (HEAD..worktree). Other modes pass
      // through unchanged.
      const fileMode: RunFilesMode =
        mode === "combined"
          ? file.lifecycle === "committed"
            ? "branch"
            : "uncommitted"
          : mode;
      onSelectFile(file, fileMode);
    },
    [onSelectFile, mode],
  );

  return (
    <div className="flex flex-col min-h-0 min-w-0 flex-1 w-full">
      <header className="flex flex-col gap-1 border-b border-border-default px-2 py-1">
        <div className="flex items-center gap-1">
          <ModeSegmented
            mode={mode}
            onChange={setUserMode}
            worktreeGone={worktreeGone}
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
            <div className="flex h-full items-center justify-center px-3 py-8 text-fg-subtle">
              <Spinner size="sm" label="Loading" />
            </div>
          ) : (
            <EmptyState message="" />
          )
        ) : !data.available ? (
          <EmptyState message={reasonLabel(data.reason)} />
        ) : tree.length === 0 ? (
          mode === "branch" && data.live ? (
            // Branch view is empty but the worktree is live — the bot
            // may still be editing without having committed yet. Prompt
            // the user toward the Uncommitted tab instead of leaving
            // them on a blank "No committed changes yet" message
            // (2026-05-21 dogfood: docs-refresh fix_claude edits show up
            // here under Uncommitted long before commit_changes runs at
            // the end of the workflow).
            <EmptyState
              message={
                <>
                  No commits in this run yet. Live edits are in the{" "}
                  <button
                    type="button"
                    className="underline text-info-fg hover:text-info-hover"
                    onClick={() => setUserMode("uncommitted")}
                  >
                    Uncommitted
                  </button>{" "}
                  tab — the workflow commits on convergence.
                </>
              }
            />
          ) : (
            <EmptyState message={emptyMessage(mode)} />
          )
        ) : (
          <div className="py-1 text-xs">
            {largeChangeset && (
              <LargeChangesetHint
                count={fileCount}
                workDir={data.work_dir}
                // Only offer inline editing while the worktree is live —
                // a finalized/gc'd run has no on-disk .gitignore to write.
                onEditGitignore={
                  onEditFile && !worktreeGone
                    ? () => onEditFile(".gitignore")
                    : undefined
                }
              />
            )}
            {tree.map((node) => (
              <TreeRow
                key={node.pathKey}
                node={node}
                collapsed={collapsed}
                onToggle={toggle}
                onSelectFile={handleSelectFile}
                showLifecycle={mode === "combined"}
              />
            ))}
          </div>
        )}
      </div>
      {data?.work_dir && (
        <footer className="border-t border-border-default px-2 py-1 text-caption text-fg-subtle truncate">
          <Tooltip content={footerTooltip(data, mode)}>
            <span className="truncate block">{footerLabel(data, mode)}</span>
          </Tooltip>
        </footer>
      )}
    </div>
  );
}

// collectFolderKeys returns every folder pathKey in the tree — used to
// auto-collapse a large changeset down to its top-level folders so the
// initial render stays bounded.
function collectFolderKeys(nodes: TreeNode[]): Set<string> {
  const keys = new Set<string>();
  const walk = (ns: TreeNode[]) => {
    for (const n of ns) {
      if (n.kind === "folder") {
        keys.add(n.pathKey);
        walk(n.children);
      }
    }
  };
  walk(nodes);
  return keys;
}

// LargeChangesetHint is a NON-blocking banner shown above the (default-
// collapsed) tree when the changeset is large. It doesn't hide anything —
// the tree renders below — it just explains the collapse and nudges the
// operator to gitignore a stray build/cache dir, pointing at the worktree
// so they can go fix it (or open it in an editor once that lands).
function LargeChangesetHint({
  count,
  workDir,
  onEditGitignore,
}: {
  count: number;
  workDir?: string;
  // When provided, renders a one-click "Edit .gitignore" button that opens
  // the run worktree's .gitignore in an editable Monaco tab. Absent when the
  // worktree is gone (nothing on disk to edit).
  onEditGitignore?: () => void;
}) {
  return (
    <div className="mb-1 flex flex-col gap-1 border-b border-border-default bg-surface-1 px-2 py-1.5 text-micro">
      <div className="text-fg-muted">
        <span className="font-medium text-warning-fg">
          {count.toLocaleString()} changes
        </span>{" "}
        — folders collapsed to keep the view responsive; expand to drill in.
      </div>
      <div className="leading-relaxed text-fg-subtle">
        If this includes a build/cache dir (a Go module/build cache,{" "}
        <code>node_modules</code>, …) add it to <code>.gitignore</code> so
        it stops flooding the diff{workDir ? "." : "."}
      </div>
      {workDir && (
        <div className="break-all text-fg-subtle">
          Worktree: <code className="text-fg-muted">{workDir}</code>
        </div>
      )}
      {onEditGitignore && (
        <div>
          <button
            type="button"
            onClick={onEditGitignore}
            className="mt-0.5 inline-flex items-center rounded-md border border-border-default bg-surface-2 px-2 py-0.5 text-caption font-medium text-fg-default hover:bg-surface-3 focus:outline-none"
          >
            Edit .gitignore
          </button>
        </div>
      )}
    </div>
  );
}

interface ModeSegmentedProps {
  mode: ViewMode;
  onChange: (mode: ViewMode) => void;
  // True when the worktree was torn down: the uncommitted + combined views
  // can't be computed, so their segments are disabled.
  worktreeGone: boolean;
  count: number;
}

// ModeSegmented is the three-pill segmented control above the tree.
// "All changes" is the union (committed + uncommitted) and the default for
// every run; "Uncommitted" maps to git status; "Branch" to BaseCommit..HEAD.
// The count badge tracks the currently-visible mode so the active pill stays
// consistent with the tree below.
function ModeSegmented({
  mode,
  onChange,
  worktreeGone,
  count,
}: ModeSegmentedProps) {
  // Worktree torn down (a finished run whose worktree was cleaned up): the
  // uncommitted + combined views need a live worktree to compute, so only the
  // branch diff is reachable. Rather than show two greyed, unclickable
  // segments — which reads as "something is broken / where are my changes" —
  // collapse to the single reachable Branch view with an inline reason.
  if (worktreeGone) {
    return (
      <div className="inline-flex items-center gap-2 text-caption">
        <div className="inline-flex overflow-hidden rounded-md border border-border-default">
          <SegmentButton
            active
            onClick={() => onChange("branch")}
            label="Branch"
            tooltip="All changes this run introduced, vs. the source branch (base..HEAD)."
            count={count}
          />
        </div>
        <span className="text-fg-subtle" title={WORKTREE_GONE_TIP}>
          worktree cleaned up — branch diff only
        </span>
      </div>
    );
  }
  return (
    <div className="inline-flex overflow-hidden rounded-md border border-border-default text-caption">
      <SegmentButton
        active={mode === "combined"}
        onClick={() => onChange("combined")}
        label="All changes"
        tooltip="All changes this run made — committed commits plus uncommitted working-tree edits, vs. the source branch. Tinted by status."
        count={mode === "combined" ? count : undefined}
      />
      <SegmentButton
        active={mode === "uncommitted"}
        onClick={() => onChange("uncommitted")}
        label="Uncommitted"
        tooltip="Files modified but not yet committed (git status)."
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
  // Active gets a solid accent fill (the app's primary affordance, as on
  // Button/IconButton) so the selected segment is unmistakable; inactive
  // hover is only a subtle surface tint. These must stay visually distinct
  // — an earlier version used bg-surface-2 for BOTH active and inactive
  // hover, which made the selected tab indistinguishable on hover.
  const cls = active
    ? "bg-accent text-fg-onAccent font-medium"
    : "bg-transparent text-fg-muted hover:bg-surface-2 hover:text-fg-default";
  return (
    <Tooltip content={tooltip}>
      <button
        type="button"
        onClick={onClick}
        disabled={disabled}
        aria-pressed={active}
        className={`px-2 py-0.5 disabled:opacity-50 disabled:cursor-not-allowed focus:outline-none ${cls}`}
      >
        <span>{label}</span>
        {typeof count === "number" && (
          <span
            className={`ml-1 inline-flex items-center justify-center rounded-md px-1 text-[9px] font-medium ${
              active ? "bg-black/20 text-fg-onAccent" : "bg-surface-3 text-fg-muted"
            }`}
          >
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
  // When true (combined mode), file rows paint a subtle committed-vs-
  // uncommitted tint keyed by file.lifecycle.
  showLifecycle: boolean;
}

function TreeRow({
  node,
  collapsed,
  onToggle,
  onSelectFile,
  showLifecycle,
}: TreeRowProps) {
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
              showLifecycle={showLifecycle}
            />
          ))}
      </>
    );
  }
  return (
    <FileRow
      node={node}
      onSelectFile={onSelectFile}
      showLifecycle={showLifecycle}
    />
  );
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
        <span className="ml-auto shrink-0 pl-2 text-caption tabular-nums">
          {folder.added > 0 && (
            <span className="text-success-fg">+{folder.added}</span>
          )}
          {folder.added > 0 && folder.deleted > 0 && (
            <span className="text-fg-subtle"> </span>
          )}
          {folder.deleted > 0 && (
            <span className="text-danger-fg">-{folder.deleted}</span>
          )}
        </span>
      )}
    </button>
  );
}

// LIFECYCLE_META drives the subtle per-file committed-vs-uncommitted
// distinction in combined mode: a faint row tint (warm for in-flight
// uncommitted work, cool for landed commits) plus a tooltip/aria suffix.
// Kept low-alpha so it reads as a hint, not a highlight, and so the
// hover:bg-surface-2 still wins on hover.
const LIFECYCLE_META: Record<
  "committed" | "uncommitted",
  { row: string; label: string }
> = {
  uncommitted: { row: "bg-warning/10", label: "uncommitted · pending" },
  committed: { row: "bg-info/[0.07]", label: "committed · on branch" },
};

function FileRow({
  node,
  onSelectFile,
  showLifecycle,
}: {
  node: TreeFile;
  onSelectFile: (file: RunFile) => void;
  showLifecycle: boolean;
}) {
  const f = node.file;
  const meta = showLifecycle && f.lifecycle ? LIFECYCLE_META[f.lifecycle] : null;
  const base = f.old_path ? `${f.old_path} → ${f.path}` : f.path;
  const tooltip = meta ? `${base} · ${meta.label}` : base;
  return (
    <Tooltip content={tooltip}>
      <button
        type="button"
        onClick={() => onSelectFile(f)}
        aria-label={meta ? `${node.label} — ${meta.label}` : undefined}
        style={{
          paddingLeft:
            INDENT_BASE + node.depth * INDENT_PER_LEVEL + CHEVRON_SLOT,
        }}
        className={`flex w-full items-center gap-2 py-0.5 pr-2 text-left hover:bg-surface-2 focus:bg-surface-2 focus:outline-none ${
          meta ? meta.row : ""
        }`}
      >
        <StatusDot status={f.status} />
        <span className="truncate min-w-0 text-fg-default">{node.label}</span>
        <span className="ml-auto shrink-0 pl-2 text-caption tabular-nums">
          {f.binary ? (
            <span className="text-fg-subtle">(binary)</span>
          ) : (
            <>
              <span className="text-success-fg">+{f.added}</span>
              <span className="text-fg-subtle"> | </span>
              <span className="text-danger-fg">-{f.deleted}</span>
            </>
          )}
        </span>
      </button>
    </Tooltip>
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
      return "Worktree was cleaned up after the run finished. Showing branch view.";
    default:
      return reason ?? "Files unavailable";
  }
}

function emptyMessage(mode: ViewMode): string {
  switch (mode) {
    case "branch":
      return "No committed changes yet";
    case "combined":
      return "No changes yet — nothing committed or pending.";
    default:
      return "No uncommitted changes — the agent hasn't touched any tracked file yet.";
  }
}

// isLive defaults to true when the field is absent so pre-feature
// backends keep their original wording (badges interpreted as
// uncommitted state).
function isLive(data: { live?: boolean }): boolean {
  return data.live !== false;
}

// footerLabel disambiguates the visible mode + lifecycle:
// - combined on a live worktree → "All run changes · worktree: name"
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
  const where = data.worktree ? "worktree" : "cwd";
  if (mode === "combined") {
    return `All run changes · ${where}: ${basename(data.work_dir ?? "")}`;
  }
  if (mode === "branch") {
    if (isLive(data)) {
      return `Branch vs. source · ${where}: ${basename(data.work_dir ?? "")}`;
    }
    return "Committed in this run";
  }
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
  if (mode === "combined") {
    return `Every file this run has touched — committed commits (BaseCommit..HEAD) plus uncommitted working-tree edits, tinted by status. Worktree: ${
      data.work_dir ?? "<unknown>"
    }`;
  }
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
