import { memo } from "react";

import type { RunSummary } from "@/api/runs";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { LiveDot } from "@/components/ui/LiveDot";
import { clickableRowProps } from "@/lib/a11y";
import { formatRelative } from "@/lib/format";

import { STATUS_VARIANT, labelForStatus } from "../runStatusMeta";
import { isResumable } from "../runStatusActions";

import { BotAvatar } from "./BotAvatar";
import {
  formatDuration,
  friendlyLabel,
  shortRunID,
  workflowDisplay,
} from "./runListFormat";
import { SourceBadge } from "./SourceBadge";

export const RunListCard = memo(function RunListCard({
  run,
  resuming,
  onOpen,
  onFilterBot,
  onResume,
}: {
  run: RunSummary;
  resuming: boolean;
  onOpen: (id: string) => void;
  onFilterBot: (botKey: string) => void;
  onResume: (id: string) => void;
}) {
  // A div (not a <button>) so the bot-avatar button can nest validly;
  // keyboard semantics are restored via clickableRowProps (role + Enter/Space).
  return (
    <div
      {...clickableRowProps(() => onOpen(run.id), friendlyLabel(run))}
      className="w-full text-left px-4 py-3 flex flex-col gap-1 min-h-[44px] cursor-pointer hover:bg-surface-2 active:bg-surface-3"
    >
      <div className="flex items-center gap-2 min-w-0 flex-wrap">
        <BotAvatar run={run} onFilter={onFilterBot} />
        <Badge variant={STATUS_VARIANT[run.status]}>
          {labelForStatus(run.status)}
        </Badge>
        <SourceBadge run={run} />
        {run.active && (
          <LiveDot tone="live" size="sm" label="Active in this process" />
        )}
        <span className="font-medium truncate">
          {friendlyLabel(run)}
        </span>
        {/* Permanent (not hover-revealed) on cards — touch has no hover. */}
        {isResumable(run.status) && (
          <Button
            variant="secondary"
            size="sm"
            className="ml-auto h-6 px-2 text-caption"
            loading={resuming}
            onClick={(e) => {
              e.stopPropagation();
              onResume(run.id);
            }}
            // Keep Enter/Space on the button from bubbling to the card's
            // clickableRowProps handler (which would navigate instead).
            onKeyDown={(e) => e.stopPropagation()}
            title="Resume from the last checkpoint"
          >
            Resume
          </Button>
        )}
      </div>
      {workflowDisplay(run) && (
        <div className="text-micro text-fg-default truncate">
          {workflowDisplay(run)}
        </div>
      )}
      <div className="text-micro text-fg-muted flex flex-wrap gap-x-2">
        <span>{formatRelative(run.created_at)}</span>
        <span>·</span>
        <span>{formatDuration(run.created_at, run.finished_at)}</span>
      </div>
      <div
        className="text-caption text-fg-subtle font-mono truncate"
        title={run.id}
      >
        {shortRunID(run.id)}
      </div>
    </div>
  );
});
