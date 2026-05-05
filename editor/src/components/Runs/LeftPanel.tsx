import { useCallback, useState } from "react";
import {
  ChevronLeftIcon,
  CommitIcon,
  FileTextIcon,
} from "@radix-ui/react-icons";

import { IconButton, Tabs, Tooltip } from "@/components/ui";
import { readBooleanFlag, writeBooleanFlag } from "@/lib/localStorageFlag";
import type { RunFile, RunHeader } from "@/api/runs";

import FilesPanel from "./FilesPanel";
import CommitsPanel from "./CommitsPanel";

// Collapsed mirrors VSCode's activity bar (~36px); expanded matches the
// source-control panel's default. Drag-to-resize is deliberately omitted
// — collapse/expand covers the 90% case and keeps the panel predictable.
const COLLAPSED_PX = 36;
const EXPANDED_PX = 320;
const COLLAPSED_KEY = "run-console-v1.left-collapsed";
const ACTIVE_TAB_KEY = "run-console-v1.left-tab";

type LeftTab = "files" | "commits";

function readActiveTab(): LeftTab {
  if (typeof window === "undefined") return "files";
  const raw = window.localStorage.getItem(ACTIVE_TAB_KEY);
  return raw === "commits" ? "commits" : "files";
}

interface LeftPanelProps {
  runId: string;
  run: RunHeader | null;
  onSelectFile: (file: RunFile) => void;
  onMergeComplete?: () => void;
}

// LeftPanel owns the chrome (collapse/expand, tab strip, footer) and
// delegates content rendering to the per-tab components. The two tabs
// are mounted unconditionally so each one's data hook keeps its WS-
// driven refresh going even when the other is visible — the cost is
// negligible compared to the UX of seeing the count badges stay live.
export default function LeftPanel({
  runId,
  run,
  onSelectFile,
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
    const v = next === "commits" ? "commits" : "files";
    setActiveTab(v);
    if (typeof window !== "undefined") {
      window.localStorage.setItem(ACTIVE_TAB_KEY, v);
    }
  }, []);

  if (collapsed) {
    return (
      <aside
        style={{ width: COLLAPSED_PX }}
        className="flex flex-col items-center border-r border-border-default bg-surface-1 py-2 gap-2 shrink-0"
      >
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
      </aside>
    );
  }

  return (
    <aside
      style={{ width: EXPANDED_PX }}
      className="flex flex-col border-r border-border-default bg-surface-1 shrink-0 min-h-0"
    >
      <div className="flex items-center border-b border-border-default">
        <Tabs
          value={activeTab}
          onValueChange={onTabChange}
          items={[
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
            onClick={toggleCollapsed}
          >
            <ChevronLeftIcon />
          </IconButton>
        </div>
      </div>
      {/* Both tabs mount unconditionally so live refresh keeps running
          on the inactive one. We just toggle visibility. */}
      <div
        className="flex-1 min-h-0"
        style={{ display: activeTab === "files" ? "flex" : "none" }}
      >
        <FilesPanel runId={runId} onSelectFile={onSelectFile} />
      </div>
      <div
        className="flex-1 min-h-0"
        style={{ display: activeTab === "commits" ? "flex" : "none" }}
      >
        <CommitsPanel
          runId={runId}
          run={run}
          onMergeComplete={onMergeComplete}
        />
      </div>
    </aside>
  );
}
