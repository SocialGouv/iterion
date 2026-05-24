import { ExternalLinkIcon } from "@radix-ui/react-icons";
import { Link } from "wouter";

import type { RunStatus } from "@/api/runs";
import { Badge } from "@/components/ui";
import { ThinkingIndicator } from "@/components/ui/ThinkingIndicator";
import { GENERIC_THINKING_WORDS } from "@/lib/thinkingWords";
import { labelForStatus } from "@/components/Runs/runStatusMeta";

interface Props {
  // Set once the launch round-trip returns a run_id.
  runId: string | null;
  // Raw RunStatus from the snapshot, if known.
  runStatus: RunStatus | null;
}

// PreFlightPanel is what fills the chat body before any whats-next-known
// node has fired its first banner. It shares the ThinkingIndicator with
// the Runs/logs ThinkingFooter so the loading aesthetic is consistent
// across the studio — same typing animation, same ✻ glyph, same mono
// italic styling. The status pill + console link live in a small inline
// row underneath so the operator still has the real-state signal (queued
// vs running vs awaiting-human) and an escape hatch to the full run
// console when something looks stuck.
export default function PreFlightPanel({ runId, runStatus }: Props) {
  return (
    <div className="mx-auto max-w-md px-4 py-10 space-y-4">
      <ThinkingIndicator
        words={GENERIC_THINKING_WORDS}
        active
        className="font-mono text-[13px] text-info-fg italic"
      />
      <div className="flex items-center gap-2 text-[11px]">
        {runStatus && <RunStatusPill status={runStatus} />}
        {runId && (
          <code className="font-mono text-fg-subtle truncate">{runId}</code>
        )}
        <span className="ml-auto" />
        {runId && (
          <Link
            href={`/runs/${encodeURIComponent(runId)}`}
            className="inline-flex items-center gap-1 text-accent hover:underline"
          >
            <ExternalLinkIcon className="w-3 h-3" />
            console
          </Link>
        )}
      </div>
      <p className="text-[10px] text-fg-subtle">
        WhatsNext streams the high-level steps here. The full run console
        (logs, executions, tool I/O) stays one click away.
      </p>
    </div>
  );
}

function RunStatusPill({ status }: { status: RunStatus }) {
  const label = labelForStatus(status);
  switch (status) {
    case "queued":
      return (
        <Badge variant="info" size="sm">
          {label}
        </Badge>
      );
    case "running":
      return (
        <Badge variant="accent" size="sm">
          {label}
        </Badge>
      );
    case "paused_waiting_human":
    case "failed_resumable":
      return (
        <Badge variant="warning" size="sm">
          {label}
        </Badge>
      );
    case "failed":
      return (
        <Badge variant="danger" size="sm">
          {label}
        </Badge>
      );
    case "cancelled":
      return (
        <Badge variant="neutral" size="sm">
          {label}
        </Badge>
      );
    case "finished":
      return (
        <Badge variant="success" size="sm">
          {label}
        </Badge>
      );
    default:
      return (
        <Badge variant="neutral" size="sm">
          {label}
        </Badge>
      );
  }
}
