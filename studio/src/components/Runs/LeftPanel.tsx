import { useCallback, useState } from "react";
import {
  ChevronLeftIcon,
  CommitIcon,
  FileTextIcon,
  InfoCircledIcon,
  ReaderIcon,
} from "@radix-ui/react-icons";

import { IconButton, Tabs, Tooltip } from "@/components/ui";
import {
  readBooleanFlag,
  readEnumFlag,
  writeBooleanFlag,
  writeStringFlag,
} from "@/lib/localStorageFlag";
import type { RunFile, RunFilesMode, RunHeader } from "@/api/runs";

import FilesPanel from "./FilesPanel";
import CommitsPanel from "./CommitsPanel";
import InfoPanel from "./InfoPanel";
import OverviewPanel from "./leftPanel/OverviewPanel";

// Collapsed mirrors VSCode's activity bar (~36px); expanded matches the
// source-control panel's default. Drag-to-resize is deliberately omitted
// — collapse/expand covers the 90% case and keeps the panel predictable.
const COLLAPSED_PX = 36;
const EXPANDED_PX = 320;
const COLLAPSED_KEY = "run-console-v1.left-collapsed";
const ACTIVE_TAB_KEY = "run-console-v1.left-tab";

// Overview leads: the run's launch config (axis, inputs, launch flags)
// is what an operator needs to orient BEFORE reading the diffs. Files
// and Commits follow because they're the run's OUTPUT; Info sits last
// as the runtime-details detail sheet. Existing operators with a
// persisted tab (files/commits/info) keep their preference — only
// fresh installs default to Overview.
const LEFT_TABS = ["overview", "files", "commits", "info"] as const;
type LeftTab = (typeof LEFT_TABS)[number];

function readActiveTab(): LeftTab {
  return readEnumFlag(ACTIVE_TAB_KEY, LEFT_TABS, "overview");
}

interface LeftPanelProps {
  runId: string;
  run: RunHeader | null;
  onSelectFile: (file: RunFile, mode: RunFilesMode) => void;
  // Opens the worktree file at `path` in an editable Monaco tab. Used by the
  // large-changeset banner's "Edit .gitignore" shortcut.
  onEditFile?: (path: string) => void;
  onMergeComplete?: () => void;
}

// LeftPanel owns the chrome (collapse/expand, tab strip, footer) and
// delegates content rendering to the per-tab components. The three
// data-heavy tabs (Files, Commits, Info) mount unconditionally so
// their WS-driven refresh keeps going even when hidden — the cost is
// negligible compared to the UX of seeing count badges stay live.
// Overview is pure props off `run`, mounted alongside the rest.
export default function LeftPanel({
  runId,
  run,
  onSelectFile,
  onEditFile,
  onMergeComplete,
}: LeftPanelProps) {
  const [collapsed, setCollapsed] = useState<boolean>(() =>
    readBooleanFlag(COLLAPSED_KEY, true),
  );
  const [activeTab, setActiveTab] = useState<LeftTab>(() => readActiveTab());

  const toggleCollapsed = useCallback(() => {
    setCollapsed((prev) => {
      const next = !prev;
      writeBooleanFlag(COLLAPSED_KEY, next);
      return next;
    });
  }, []);

  const onTabChange = useCallback((next: string) => {
    const v: LeftTab = (LEFT_TABS as readonly string[]).includes(next)
      ? (next as LeftTab)
      : "overview";
    setActiveTab(v);
    writeStringFlag(ACTIVE_TAB_KEY, v);
  }, []);

  if (collapsed) {
    return (
      <aside
        style={{ width: COLLAPSED_PX }}
        className="flex flex-col items-center border-r border-border-default bg-surface-1 py-2 gap-2 shrink-0"
      >
        <Tooltip content="Show overview">
          <button
            type="button"
            onClick={() => {
              setActiveTab("overview");
              toggleCollapsed();
            }}
            aria-label="Show overview"
            className="relative inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-muted hover:bg-surface-2 hover:text-fg-default"
          >
            <ReaderIcon />
          </button>
        </Tooltip>
        <Tooltip content="Show files">
          <button
            type="button"
            onClick={() => {
              setActiveTab("files");
              toggleCollapsed();
            }}
            aria-label="Show files"
            className="relative inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-muted hover:bg-surface-2 hover:text-fg-default"
          >
            <FileTextIcon />
          </button>
        </Tooltip>
        <Tooltip content="Show commits">
          <button
            type="button"
            onClick={() => {
              setActiveTab("commits");
              toggleCollapsed();
            }}
            aria-label="Show commits"
            className="relative inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-muted hover:bg-surface-2 hover:text-fg-default"
          >
            <CommitIcon />
          </button>
        </Tooltip>
        <Tooltip content="Show run info">
          <button
            type="button"
            onClick={() => {
              setActiveTab("info");
              toggleCollapsed();
            }}
            aria-label="Show run info"
            className="relative inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-muted hover:bg-surface-2 hover:text-fg-default"
          >
            <InfoCircledIcon />
          </button>
        </Tooltip>
      </aside>
    );
  }

  return (
    <aside
      style={{ width: EXPANDED_PX }}
      className="flex flex-col border-r border-border-default bg-surface-1 shrink-0 min-h-0 overflow-hidden"
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
            {
              value: "info",
              label: "Info",
              icon: <InfoCircledIcon className="h-3.5 w-3.5" />,
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
            onClick={toggleCollapsed}
          >
            <ChevronLeftIcon />
          </IconButton>
        </div>
      </div>
      {/* The three data-heavy tabs (Files, Commits, Info) mount
          unconditionally so live refresh keeps running on the inactive
          ones. The hidden tab body is collapsed via `hidden`
          (display: none) — using flex-col on the visible one so the
          inner panel's `flex-1` grows on the column axis (height) and
          its width is bounded by the aside's stretch (no horizontal
          overflow when a file path is wider than the panel). Overview
          is a pure projection of `run`, no live subscription — safe
          to mount alongside the rest. */}
      <div
        className={
          activeTab === "overview"
            ? "flex-1 min-h-0 min-w-0 flex flex-col"
            : "hidden"
        }
      >
        <OverviewPanel run={run} />
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
      <div
        className={
          activeTab === "info"
            ? "flex-1 min-h-0 min-w-0 flex flex-col"
            : "hidden"
        }
      >
        <InfoPanel run={run} />
      </div>
    </aside>
  );
}
