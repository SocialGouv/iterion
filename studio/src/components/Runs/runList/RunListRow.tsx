import { memo } from "react";
import { attachReferenceById } from "@/lib/chatDock/attachToAssistant";
import { referenceDragProps } from "@/lib/chatDock/dragReference";
import { pointerRowProps } from "@/lib/a11y";

import type { RunSummary } from "@/api/runs";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { LiveDot } from "@/components/ui/LiveDot";
import { compactPath } from "@/lib/compactPath";
import { formatRelative } from "@/lib/format";

import { STATUS_VARIANT, labelForStatus } from "../runStatusMeta";
import { isResumable } from "../runStatusActions";

import { BotAvatar } from "./BotAvatar";
import {
  formatDuration,
  friendlyLabel,
  runRowQualifier,
  shortRunID,
  workflowDisplay,
} from "./runListFormat";
import { SourceBadge } from "./SourceBadge";

// Memoised so the parent's per-row callback (now stable via useCallback)
// doesn't force every row to re-render when one run mutates.
export const RunListRow = memo(function RunListRow({
  run,
  selected,
  resuming,
  onOpen,
  onFilterBot,
  onToggleSelect,
  onResume,
}: {
  run: RunSummary;
  selected: boolean;
  resuming: boolean;
  onOpen: (id: string) => void;
  onFilterBot: (botKey: string) => void;
  onToggleSelect: (id: string) => void;
  onResume: (id: string) => void;
}) {
  return (
    // The row's click is a pointer convenience — it holds the selection
    // checkbox, the attach grip, the bot avatar and Resume, so it carries
    // no widget role (that would be `nested-interactive`, WCAG 4.1.2). The
    // keyboard path to open the run is the title button in the second cell.
    <tr
      className="group border-b border-border-default hover:bg-surface-2 cursor-pointer"
      {...pointerRowProps(() => onOpen(run.id))}
    >
      {/* Selection cell swallows its clicks so toggling never navigates. */}
      <td className="pl-4 pr-1 py-2 w-8" onClick={(e) => e.stopPropagation()}>
        <Checkbox
          aria-label={`Select run ${friendlyLabel(run)} ${runRowQualifier(run)}`}
          checked={selected}
          onChange={() => onToggleSelect(run.id)}
        />
      </td>
      <td className="px-4 py-2">
        <div className="flex items-center gap-2">
          {/* The grip is the attach control (#2770, option A): a real
              button, so the tab order reaches it and Enter/Space attach —
              the keyboard and single-pointer path SC 2.1.1 / 2.5.7 owed.
              The drag still rides the same element, and the click mints
              the same reference the drop of this grip would. */}
          <button
            type="button"
            className="shrink-0 cursor-grab select-none text-fg-muted focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
            aria-label={`Attach ${friendlyLabel(run)} ${runRowQualifier(run)} to the assistant`}
            title="Attach to the assistant, or drag onto it, to ask about this run"
            {...referenceDragProps("run", run.id, friendlyLabel(run))}
            onClick={(e) => {
              // The row's own click opens the run — the attach must not.
              e.stopPropagation();
              attachReferenceById("run", run.id, friendlyLabel(run));
            }}
          >
            ⋮⋮
          </button>
          <BotAvatar run={run} onFilter={onFilterBot} />
          {/* The row's action, as a real control: same name shape as this
              row's Resume, because friendlyLabel falls back to
              workflow_name and the short id is what separates two runs of
              one workflow. Visible word first, for SC 2.5.3. */}
          <button
            type="button"
            className="font-medium truncate text-left hover:underline focus-visible:underline"
            onClick={(e) => {
              e.stopPropagation();
              onOpen(run.id);
            }}
            aria-label={`Open ${friendlyLabel(run)} ${runRowQualifier(run)}`}
          >
            {friendlyLabel(run)}
          </button>
        </div>
      </td>
      <td className="px-4 py-2">
        {workflowDisplay(run) && (
          <div className="text-fg-default">{workflowDisplay(run)}</div>
        )}
        {run.file_path && (
          <div
            className="text-fg-subtle text-caption truncate max-w-md"
            title={run.file_path}
          >
            {compactPath(run.file_path)}
          </div>
        )}
      </td>
      <td className="px-4 py-2">
        <SourceBadge run={run} />
      </td>
      <td className="px-4 py-2">
        <div className="flex items-center gap-1.5">
          <Badge variant={STATUS_VARIANT[run.status]}>
            {labelForStatus(run.status)}
          </Badge>
          {run.active && (
            <LiveDot tone="live" size="sm" label="Active in this process" />
          )}
          {isResumable(run.status) && (
            <Button
              variant="secondary"
              size="sm"
              className={`h-5 px-1.5 text-caption transition-opacity ${
                resuming
                  ? "opacity-100"
                  : "opacity-0 group-hover:opacity-100 focus-visible:opacity-100"
              }`}
              loading={resuming}
              onClick={(e) => {
                e.stopPropagation();
                onResume(run.id);
              }}
              // One Resume per row, and the label alone does not separate
              // them: friendlyLabel falls back to workflow_name, so a list of
              // `deploy` runs would read "Resume deploy" N times. The short
              // id is the row's own discriminator, and the last cell shows
              // it. Visible word first, for SC 2.5.3.
              aria-label={`Resume ${friendlyLabel(run)} ${runRowQualifier(run)}`}
              title="Resume from the last checkpoint"
            >
              Resume
            </Button>
          )}
        </div>
      </td>
      <td className="px-4 py-2 text-fg-muted">{formatRelative(run.created_at)}</td>
      <td className="px-4 py-2 text-fg-muted">
        {formatDuration(run.created_at, run.finished_at)}
      </td>
      <td className="px-4 py-2 font-mono text-caption text-fg-subtle" title={run.id}>
        {shortRunID(run.id)}
      </td>
    </tr>
  );
});
