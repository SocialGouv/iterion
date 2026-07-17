import { ExternalLinkIcon } from "@radix-ui/react-icons";
import { Link } from "wouter";

import type { RunStatus } from "@/api/runs";
import { Badge, InlineBanner } from "@/components/ui";
import { ThinkingIndicator } from "@/components/ui/ThinkingIndicator";
import { GENERIC_THINKING_WORDS } from "@/lib/thinkingWords";
import { labelForStatus } from "@/components/Runs/runStatusMeta";
import { useRunStore } from "@/store/run";

interface Props {
  // Set once the launch round-trip returns a run_id.
  runId: string | null;
  // Raw RunStatus from the snapshot, if known.
  runStatus: RunStatus | null;
}

// PreFlightPanel is what fills the chat body before any whats-next-known
// node has fired its first banner. While the run is genuinely alive it
// shares the ThinkingIndicator with the Runs/logs ThinkingFooter so the
// loading aesthetic is consistent across the studio. When the run is
// TERMINAL with zero transcript (a launch that died before the first
// node banner, or a re-attached dead session), the spinner would lie —
// render the outcome instead: what happened, the engine error when there
// is one, and how to move on (the always-on composer re-seeds a fresh
// session; failed_resumable/cancelled get the Resume footer from the
// parent view).
export default function PreFlightPanel({ runId, runStatus }: Props) {
  const runError = useRunStore((s) => s.snapshot?.run.error ?? null);
  const terminal =
    runStatus === "failed" ||
    runStatus === "finished" ||
    runStatus === "cancelled" ||
    runStatus === "failed_resumable";

  return (
    <div className="mx-auto max-w-md px-4 py-10 space-y-4">
      {terminal ? (
        <TerminalOutcome status={runStatus} error={runError} />
      ) : (
        <ThinkingIndicator
          words={GENERIC_THINKING_WORDS}
          active
          className="font-mono text-label text-info-fg italic"
        />
      )}
      <div className="flex items-center gap-2 text-micro">
        {runStatus && <RunStatusPill status={runStatus} />}
        {runId && (
          <code className="font-mono text-fg-subtle truncate">{runId}</code>
        )}
        <span className="ml-auto" />
        {runId && (
          <Link
            href={`/runs/${encodeURIComponent(runId)}`}
            className="inline-flex items-center gap-1 text-accent-text hover:underline"
          >
            <ExternalLinkIcon className="w-3 h-3" />
            console
          </Link>
        )}
      </div>
      {!terminal && (
        <p className="text-caption text-fg-subtle">
          WhatsNext streams the high-level steps here. The full run console
          (logs, executions, tool I/O) stays one click away.
        </p>
      )}
    </div>
  );
}

function TerminalOutcome({
  status,
  error,
}: {
  status: RunStatus;
  error: string | null;
}) {
  if (status === "failed" || status === "failed_resumable") {
    return (
      <InlineBanner
        tone="danger"
        layout="inline"
        title="This session failed before Nexie could say anything."
      >
        <div className="space-y-2">
          {error && (
            <code className="block whitespace-pre-wrap break-words font-mono text-micro opacity-90">
              {error}
            </code>
          )}
          <p>
            {status === "failed_resumable"
              ? "Resume below to retry from the checkpoint, or send a message to start fresh."
              : "Send a message below to start a fresh session."}
          </p>
        </div>
      </InlineBanner>
    );
  }
  const title =
    status === "cancelled"
      ? "This session was cancelled before any exchange."
      : "This session ended without any exchange.";
  return (
    <InlineBanner tone="info" layout="inline" title={title}>
      <p>Send a message below to start a fresh session.</p>
    </InlineBanner>
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
