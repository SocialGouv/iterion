import { memo } from "react";
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
    // checkbox, the bot avatar and Resume, so it carries no widget role
    // (that would be `nested-interactive`, WCAG 4.1.2). The keyboard path
    // to open the run is the title button in the second cell.
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
          {/* role="img": ARIA in HTML prohibits aria-label on a roleless
              span (axe aria-prohibited-attr), and the grip IS a glyph whose
              text alternative says what the pointer can do with it — same
              shape as ui/HelpHint. It claims no operation, because there is
              none to claim: dragging a reference has no keyboard or
              single-pointer path yet (#2770). */}
          <span
            role="img"
            className="shrink-0 cursor-grab select-none text-fg-muted"
            aria-label={`Drag ${friendlyLabel(run)} ${runRowQualifier(run)} onto the assistant`}
            title="Drag onto the assistant to ask about this run"
            {...referenceDragProps("run", run.id, friendlyLabel(run))}
          >
            ⋮⋮
          </span>
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
