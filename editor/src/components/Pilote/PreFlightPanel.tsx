import { ReloadIcon, ExternalLinkIcon } from "@radix-ui/react-icons";
import { Link } from "wouter";

import type { RunStatus } from "@/api/runs";
import { Badge } from "@/components/ui";

interface Props {
  // Set once the launch round-trip returns a run_id.
  runId: string | null;
  // High-level session status from useWhatsNextSession.
  status: "idle" | "launching" | "active" | "submitting" | "ended";
  // Raw RunStatus from the snapshot, if known.
  runStatus: RunStatus | null;
  // Total number of run events the store has consumed so far. Used to
  // pick the "waiting for first node" copy over "starting up" once we
  // know events are arriving but none have mapped to a known node yet.
  rawEventCount: number;
}

// PreFlightPanel is what fills the chat body before any whats-next-known
// node has fired its first banner. It tells the operator that the run
// is launching, queued, or waiting on a slot — and exposes a link to
// the full /runs/:id console as a fallback when something looks stuck.

export default function PreFlightPanel({
  runId,
  status,
  runStatus,
  rawEventCount,
}: Props) {
  const copy = pickCopy(status, runStatus, rawEventCount);

  return (
    <div className="mx-auto max-w-md px-4 py-10">
      <div className="rounded-lg border border-border-default bg-surface-1 p-5 space-y-4">
        <div className="flex items-start gap-3">
          <ReloadIcon
            className="w-5 h-5 text-accent shrink-0 mt-0.5 animate-spin"
            aria-hidden="true"
          />
          <div className="flex-1 space-y-1">
            <h3 className="text-[14px] font-semibold text-fg-default">
              {copy.title}
            </h3>
            <p className="text-[12px] text-fg-muted">{copy.body}</p>
          </div>
        </div>

        <div className="flex items-center gap-2 pt-3 border-t border-border-subtle">
          {runStatus && <RunStatusPill status={runStatus} />}
          {runId && (
            <code className="text-[10px] font-mono text-fg-subtle truncate">
              {runId}
            </code>
          )}
          <span className="ml-auto" />
          {runId && (
            <Link
              href={`/runs/${encodeURIComponent(runId)}`}
              className="inline-flex items-center gap-1 text-[11px] text-accent hover:underline"
            >
              <ExternalLinkIcon className="w-3 h-3" />
              console
            </Link>
          )}
        </div>
      </div>

      <p className="mt-4 text-center text-[10px] text-fg-subtle">
        Pilote streams the high-level steps here. The full run console
        (logs, executions, tool I/O) stays one click away.
      </p>
    </div>
  );
}

function pickCopy(
  status: Props["status"],
  runStatus: RunStatus | null,
  rawEventCount: number,
): { title: string; body: string } {
  if (status === "launching" || runStatus === null) {
    return {
      title: "Starting the run…",
      body: "Iterion is wiring up the executor (backend, tools, optional sandbox). The first survey step will begin in a moment.",
    };
  }
  if (runStatus === "queued") {
    return {
      title: "Queued",
      body: "Your run is in the queue. It will start as soon as a runner picks it up.",
    };
  }
  if (runStatus === "running" && rawEventCount === 0) {
    return {
      title: "Run dispatched — waiting for the first event",
      body: "The engine has started but no event has reached this view yet. If the WebSocket is reconnecting this can take a few seconds.",
    };
  }
  if (runStatus === "running") {
    return {
      title: "Iterion is preparing the first step",
      body: "Background activity is happening (model warmup, MCP servers, attachments). The Pilote chat starts as soon as the first whats-next node fires.",
    };
  }
  if (runStatus === "paused_waiting_human") {
    return {
      title: "Waiting for your input",
      body: "A human node is paused. If you don't see a chat bubble, the schema lookup is still in progress — refresh in a moment or open the full run console.",
    };
  }
  return {
    title: "Waiting…",
    body: "The run is in an intermediate state. Use the run console for the full picture.",
  };
}

function RunStatusPill({ status }: { status: RunStatus }) {
  switch (status) {
    case "queued":
      return (
        <Badge variant="info" size="sm">
          queued
        </Badge>
      );
    case "running":
      return (
        <Badge variant="accent" size="sm">
          running
        </Badge>
      );
    case "paused_waiting_human":
      return (
        <Badge variant="warning" size="sm">
          waiting
        </Badge>
      );
    case "failed_resumable":
      return (
        <Badge variant="warning" size="sm">
          retryable
        </Badge>
      );
    case "failed":
      return (
        <Badge variant="danger" size="sm">
          failed
        </Badge>
      );
    case "cancelled":
      return (
        <Badge variant="neutral" size="sm">
          cancelled
        </Badge>
      );
    case "finished":
      return (
        <Badge variant="success" size="sm">
          finished
        </Badge>
      );
    default:
      return (
        <Badge variant="neutral" size="sm">
          {status}
        </Badge>
      );
  }
}
