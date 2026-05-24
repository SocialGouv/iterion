import { useEffect, useState } from "react";

import type { RunStatus } from "@/api/runs";

interface Props {
  status: RunStatus | undefined;
  // RFC3339 anchor of the currently-accruing run window; null while
  // the run is paused/terminated. Drives the stalled-timer below.
  currentRunStart: string | undefined;
  onShowEventLog: () => void;
}

const STALLED_AFTER_MS = 30_000;

export default function ConversationEmptyState({
  status,
  currentRunStart,
  onShowEventLog,
}: Props) {
  const stalled = useStalledTimer(currentRunStart, STALLED_AFTER_MS);

  let icon = "⏳";
  let primary = "Waiting for the run to start…";
  let secondary: string | null = null;
  let showEventLogLink = false;

  switch (status) {
    case "queued":
      icon = "🕒";
      primary = "Run queued";
      secondary = "Waiting for a runner to pick it up.";
      break;
    case "running":
      icon = "⏳";
      primary = "Run started";
      if (stalled) {
        secondary =
          "The agent has been running for a while without producing conversational output. It may be reading files, calling tools, or thinking.";
        showEventLogLink = true;
      } else {
        secondary = "Waiting for the first agent output…";
      }
      break;
    case "paused_waiting_human":
      icon = "⏸";
      primary = "Run paused";
      secondary = "Waiting for a human answer.";
      break;
    case "paused_operator":
      icon = "⏸";
      primary = "Run paused by operator";
      secondary = "Use the Resume button in the header to continue.";
      break;
    case "finished":
      icon = "✓";
      primary = "Run finished";
      secondary = "No conversational messages were produced.";
      break;
    case "failed":
      icon = "⚠";
      primary = "Run failed";
      secondary =
        "No conversational messages were produced. Check the Events tab for error details and tool logs.";
      showEventLogLink = true;
      break;
    case "failed_resumable":
      icon = "⚠";
      primary = "Run failed";
      secondary =
        "No conversational messages were produced. Check the Events tab for error details, address the issue, then use Resume when ready.";
      showEventLogLink = true;
      break;
    case "cancelled":
      icon = "⊘";
      primary = "Run cancelled";
      secondary = "No conversational messages were produced.";
      break;
    default:
      // Unknown / loading status — fall back to the legacy copy.
      break;
  }

  return (
    <div className="flex h-full w-full flex-col items-center justify-center px-6 py-8 text-center">
      <div className="text-2xl mb-2 select-none" aria-hidden>
        {icon}
      </div>
      <p className="text-[13px] font-medium text-fg-default">{primary}</p>
      {secondary && (
        <p className="mt-1 max-w-xs text-[11px] text-fg-subtle leading-snug">
          {secondary}
        </p>
      )}
      {showEventLogLink && (
        <button
          type="button"
          onClick={onShowEventLog}
          className="mt-3 text-[11px] text-accent underline-offset-2 hover:underline focus:outline-none focus-visible:underline"
        >
          Show event log →
        </button>
      )}
    </div>
  );
}

function useStalledTimer(
  startedAt: string | undefined,
  thresholdMs: number,
): boolean {
  const [stalled, setStalled] = useState(false);

  useEffect(() => {
    setStalled(false);
    if (!startedAt) return;
    const startMs = Date.parse(startedAt);
    if (Number.isNaN(startMs)) return;
    const remaining = startMs + thresholdMs - Date.now();
    if (remaining <= 0) {
      setStalled(true);
      return;
    }
    const t = setTimeout(() => setStalled(true), remaining);
    return () => clearTimeout(t);
  }, [startedAt, thresholdMs]);

  return stalled;
}
