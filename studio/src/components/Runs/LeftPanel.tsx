import {
  useCallback,
  useState,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from "react";
import {
  ChevronLeftIcon,
  CommitIcon,
  FileTextIcon,
  ReaderIcon,
} from "@radix-ui/react-icons";

import { IconButton, Tabs, Tooltip } from "@/components/ui";
import { readEnumFlag, writeStringFlag } from "@/lib/localStorageFlag";
import type { RunFile, RunFilesMode, RunHeader } from "@/api/runs";

import FilesPanel from "./FilesPanel";
import CommitsPanel from "./CommitsPanel";
import OverviewPanel from "./leftPanel/OverviewPanel";

// Collapsed rail mirrors VSCode's activity bar (~36px). When expanded the
// panel fills a resizable react-resizable-panels Panel owned by RunView
// (drag-to-resize replaces the old fixed 320px), so it no longer pins its
// own width. The collapse state + its persistence also live in RunView
// (LEFT_COLLAPSED_KEY) so the layout row can drop the left Panel entirely
// while collapsed and hand the width back to the canvas.
export const LEFT_COLLAPSED_PX = 36;
export const LEFT_COLLAPSED_KEY = "run-console-v1.left-collapsed";
// Expanded panel is an explicit persisted pixel width with a drag handle
// (VS-Code sidebar style). We use a definite width rather than a
// react-resizable-panels Panel because that library's measure-based
// initial sizing collapsed a flex-1 left panel to its (tiny) content
// width; an explicit width is predictable across mounts.
export const LEFT_WIDTH_KEY = "run-console-v2.left-width";
export const LEFT_WIDTH_DEFAULT = 320;
export const LEFT_WIDTH_MIN = 220;
export const LEFT_WIDTH_MAX = 560;
const ACTIVE_TAB_KEY = "run-console-v1.left-tab";

// clampLeftWidth keeps a persisted/dragged width inside sane bounds so a
// corrupted value can't strand the panel off-screen or hair-thin.
export function clampLeftWidth(w: number): number {
  if (!Number.isFinite(w)) return LEFT_WIDTH_DEFAULT;
  return Math.min(LEFT_WIDTH_MAX, Math.max(LEFT_WIDTH_MIN, w));
}

// Overview leads: it is now the run's mission control — status, budget
// meters, progress, briefing, config, outcome, and (folded in) the raw
// details that used to be the Info tab. Files and Commits follow as the
// run's OUTPUT. A persisted "info" preference (the removed tab) falls
// back to Overview via readEnumFlag.
const LEFT_TABS = ["overview", "files", "commits"] as const;
type LeftTab = (typeof LEFT_TABS)[number];

function readActiveTab(): LeftTab {
  return readEnumFlag(ACTIVE_TAB_KEY, LEFT_TABS, "overview");
}

interface LeftPanelProps {
  runId: string;
  run: RunHeader | null;
  // Collapse + width are controlled by RunView (persisted there) so the
  // expanded panel has an explicit, predictable width with a drag handle.
  collapsed: boolean;
  onToggleCollapsed: () => void;
  width: number;
  onResize: (width: number) => void;
  onSelectFile: (file: RunFile, mode: RunFilesMode) => void;
  // Opens the worktree file at `path` in an editable Monaco tab. Used by the
  // large-changeset banner's "Edit .gitignore" shortcut.
  onEditFile?: (path: string) => void;
  onMergeComplete?: () => void;
  // Selects the first failed node on the canvas — wired down to the
  // Overview's Progress "failed" chip.
  onJumpToFailed?: (nodeId: string) => void;
}

// LeftPanel owns the tab chrome and delegates content to the per-tab
// components. Files and Commits mount unconditionally so their WS-driven
// refresh keeps going even when hidden. Overview is the mission-control
// dashboard; it has its own live subscriptions (metrics, file/commit
// counts) and mounts alongside the rest.
export default function LeftPanel({
  runId,
  run,
  collapsed,
  onToggleCollapsed,
  width,
  onResize,
  onSelectFile,
  onEditFile,
  onMergeComplete,
  onJumpToFailed,
}: LeftPanelProps) {
  const [activeTab, setActiveTab] = useState<LeftTab>(() => readActiveTab());

  // Drag-to-resize the expanded panel. Tracks pointer on window so the
  // drag keeps working when the cursor leaves the thin handle.
  const startResize = useCallback(
    (e: ReactPointerEvent) => {
      e.preventDefault();
      const startX = e.clientX;
      const startW = width;
      const onMove = (ev: PointerEvent) => {
        onResize(clampLeftWidth(startW + (ev.clientX - startX)));
      };
      const onUp = () => {
        window.removeEventListener("pointermove", onMove);
        window.removeEventListener("pointerup", onUp);
        document.body.style.cursor = "";
        document.body.style.userSelect = "";
      };
      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";
      window.addEventListener("pointermove", onMove);
      window.addEventListener("pointerup", onUp);
    },
    [width, onResize],
  );

  const onTabChange = useCallback((next: string) => {
    const v: LeftTab = (LEFT_TABS as readonly string[]).includes(next)
      ? (next as LeftTab)
      : "overview";
    setActiveTab(v);
    writeStringFlag(ACTIVE_TAB_KEY, v);
  }, []);

  const expandTo = useCallback(
    (tab: LeftTab) => {
      setActiveTab(tab);
      onToggleCollapsed();
    },
    [onToggleCollapsed],
  );

  if (collapsed) {
    return (
      <aside
        style={{ width: LEFT_COLLAPSED_PX }}
        className="flex flex-col items-center border-r border-border-default bg-surface-1 py-2 gap-2 shrink-0"
      >
        <RailButton label="Show overview" onClick={() => expandTo("overview")}>
          <ReaderIcon />
        </RailButton>
        <RailButton label="Show files" onClick={() => expandTo("files")}>
          <FileTextIcon />
        </RailButton>
        <RailButton label="Show commits" onClick={() => expandTo("commits")}>
          <CommitIcon />
        </RailButton>
      </aside>
    );
  }

  return (
    <aside
      style={{ width }}
      className="relative flex flex-col border-r border-border-default bg-surface-1 min-h-0 shrink-0 overflow-hidden"
    >
      <div className="flex items-center border-b border-border-default">
        <Tabs
          value={activeTab}
          onValueChange={onTabChange}
          items={[
            {
              value: "overview",
              label: "Overview",
              icon: <ReaderIcon className="h-3.5 w-3.5" />,
            },
            {
              value: "files",
              label: "Files",
              icon: <FileTextIcon className="h-3.5 w-3.5" />,
            },
            {
              value: "commits",
              label: "Commits",
              icon: <CommitIcon className="h-3.5 w-3.5" />,
            },
          ]}
          variant="underline"
          listClassName="flex-1 px-1"
          className="flex-1"
        />
        <div className="px-1">
          <IconButton
            label="Hide panel"
            size="sm"
            variant="ghost"
            onClick={onToggleCollapsed}
          >
            <ChevronLeftIcon />
          </IconButton>
        </div>
      </div>
      {/* All tabs mount unconditionally so live refresh keeps running on
          the inactive ones (Files/Commits WS-driven counts; Overview's
          own metrics + file/commit-count subscriptions). The hidden tab
          body is collapsed via `hidden` (display: none) — flex-col on the
          visible one so the inner panel's `flex-1` grows on the column
          axis (height) and its width is bounded by the aside's stretch. */}
      <div
        className={
          activeTab === "overview"
            ? "flex-1 min-h-0 min-w-0 flex flex-col"
            : "hidden"
        }
      >
        <OverviewPanel
          runId={runId}
          run={run}
          onSwitchTab={onTabChange}
          onJumpToFailed={onJumpToFailed}
        />
      </div>
      <div
        className={
          activeTab === "files"
            ? "flex-1 min-h-0 min-w-0 flex flex-col"
            : "hidden"
        }
      >
        <FilesPanel
          runId={runId}
          onSelectFile={onSelectFile}
          onEditFile={onEditFile}
        />
      </div>
      <div
        className={
          activeTab === "commits"
            ? "flex-1 min-h-0 min-w-0 flex flex-col"
            : "hidden"
        }
      >
        <CommitsPanel
          runId={runId}
          run={run}
          onMergeComplete={onMergeComplete}
        />
      </div>
      {/* Drag handle on the right edge to resize the panel. */}
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize panel"
        onPointerDown={startResize}
        title="Drag to resize"
        className="absolute inset-y-0 right-0 w-1 cursor-col-resize hover:bg-accent/60 active:bg-accent transition-colors"
      />
    </aside>
  );
}

// RailButton is one collapsed-rail activity-bar icon that expands the
// panel to its tab.
function RailButton({
  label,
  onClick,
  children,
}: {
  label: string;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <Tooltip content={label}>
      <button
        type="button"
        onClick={onClick}
        aria-label={label}
        className="relative inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-muted hover:bg-surface-2 hover:text-fg-default"
      >
        {children}
      </button>
    </Tooltip>
  );
}
