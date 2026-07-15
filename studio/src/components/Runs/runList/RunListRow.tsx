import { memo } from "react";

import type { RunSummary } from "@/api/runs";
import { Badge } from "@/components/ui/Badge";
import { LiveDot } from "@/components/ui/LiveDot";
import { compactPath } from "@/lib/compactPath";
import { formatRelative } from "@/lib/format";

import { STATUS_VARIANT, labelForStatus } from "../runStatusMeta";

import { BotAvatar } from "./BotAvatar";
import {
  formatDuration,
  friendlyLabel,
  shortRunID,
  workflowDisplay,
} from "./runListFormat";
import { SourceBadge } from "./SourceBadge";

// Memoised so the parent's per-row callback (now stable via useCallback)
// doesn't force every row to re-render when one run mutates.
export const RunListRow = memo(function RunListRow({
  run,
  onOpen,
  onFilterBot,
}: {
  run: RunSummary;
  onOpen: (id: string) => void;
  onFilterBot: (botKey: string) => void;
}) {
  return (
    <tr
      className="border-b border-border-default hover:bg-surface-2 cursor-pointer"
      onClick={() => onOpen(run.id)}
    >
      <td className="px-4 py-2">
        <div className="flex items-center gap-2">
          <BotAvatar run={run} onFilter={onFilterBot} />
          <span className="font-medium truncate">{friendlyLabel(run)}</span>
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
        <Badge variant={STATUS_VARIANT[run.status]}>
          {labelForStatus(run.status)}
        </Badge>
        {run.active && (
          <LiveDot
            tone="live"
            size="sm"
            className="ml-1.5"
            label="Active in this process"
          />
        )}
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
