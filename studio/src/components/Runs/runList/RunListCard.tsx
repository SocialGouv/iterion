import { memo } from "react";

import type { RunSummary } from "@/api/runs";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { LiveDot } from "@/components/ui/LiveDot";
import { pointerRowProps } from "@/lib/a11y";
import { formatRelative } from "@/lib/format";

import { STATUS_VARIANT, labelForStatus } from "../runStatusMeta";
import { isResumable } from "../runStatusActions";

import { BotAvatar } from "./BotAvatar";
import {
  formatDuration,
  friendlyLabel,
  retryAttemptLabel,
  retryHint,
  runRowQualifier,
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
  // The card holds the bot-avatar and Resume buttons, so it carries no
  // widget role of its own: a role="button" around them is
  // `nested-interactive` (WCAG 4.1.2). Its click stays a pointer
  // convenience, and the keyboard path to open the run is the title
  // button below — one tab stop per action, none for the card itself.
  return (
    <div
      {...pointerRowProps(() => onOpen(run.id))}
      className="w-full text-left px-4 py-3 flex flex-col gap-1 min-h-[44px] cursor-pointer hover:bg-surface-2 active:bg-surface-3"
    >
      <div className="flex items-center gap-2 min-w-0 flex-wrap">
        <BotAvatar run={run} onFilter={onFilterBot} />
        <Badge variant={STATUS_VARIANT[run.status]}>
          {labelForStatus(run.status)}
        </Badge>
        {/* A parked run is waiting, not dead — say so next to the status
            that would otherwise read as final. */}
        {retryHint(run) && (
          <Badge variant="info" title={retryAttemptLabel(run)}>
            {retryHint(run)}
          </Badge>
        )}
        <SourceBadge run={run} />
        {run.active && (
          <LiveDot tone="live" size="sm" label="Active in this process" />
        )}
        {/* The card's own action, as a real control: the keyboard reaches
            the run here, and the accessible name carries the short id
            because friendlyLabel falls back to workflow_name and a list of
            `deploy` runs would otherwise offer N identical controls.
            Visible word first, for SC 2.5.3. */}
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
            // One Resume per card, qualified like the row's: friendlyLabel
            // falls back to workflow_name, so the short id the card also
            // shows is what separates two runs of one workflow.
            aria-label={`Resume ${friendlyLabel(run)} ${runRowQualifier(run)}`}
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
