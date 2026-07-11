import { CheckCircledIcon, ReloadIcon, CrossCircledIcon } from "@radix-ui/react-icons";

import type { BannerMessage, BannerStatus } from "@/lib/runChat/types";

interface Props {
  message: BannerMessage;
}

// BannerCard renders a single node's lifecycle as a chat row.
// The NodeOutputCard immediately below carries the full output, so
// we don't render an inline summary here — keeps the banner row tight.
export default function BannerCard({ message }: Props) {
  const { label, status, errorMessage, nodeId, progress } = message;
  return (
    <div className="flex items-start gap-2 text-body">
      <div className="mt-0.5 shrink-0">
        <BannerStatusIcon status={status} />
      </div>
      <div className="flex-1 min-w-0">
        <div className="flex items-baseline gap-2">
          <span className="text-fg-default">{label}</span>
          {label !== nodeId && (
            <span className="text-caption font-mono text-fg-subtle">{nodeId}</span>
          )}
        </div>
        {progress && status === "running" && <BannerProgressLine progress={progress} />}
        {errorMessage && status === "failed" && (
          <p className="mt-1 text-micro text-danger-fg">{errorMessage}</p>
        )}
      </div>
    </div>
  );
}

// BannerProgressLine is the "12 tool calls · latest: read_file —
// README.md" line under a running banner. Exported so the whats-next
// NodeBanner can re-use it without duplicating the truncation rules.
export function BannerProgressLine({
  progress,
}: {
  progress: NonNullable<BannerMessage["progress"]>;
}) {
  const retryCount = progress.retryCount ?? 0;
  if (progress.toolCount === 0 && retryCount === 0) return null;
  const toolNoun = progress.toolCount === 1 ? "tool call" : "tool calls";
  const retryNoun = retryCount === 1 ? "retry" : "retries";
  return (
    <p className="mt-1 text-micro text-fg-muted truncate">
      {progress.toolCount > 0 && (
        <>
          <span className="font-mono">{progress.toolCount}</span> {toolNoun}
          {progress.latestTool && (
            <>
              {" "}· latest:{" "}
              <code className="text-micro font-mono text-fg-default">
                {progress.latestTool}
              </code>
              {progress.latestToolHint &&
                progress.latestToolHint !== progress.latestTool && (
                  <span className="text-fg-subtle"> — {progress.latestToolHint}</span>
                )}
            </>
          )}
        </>
      )}
      {retryCount > 0 && (
        <span
          className="ml-2 inline-flex items-center gap-1 text-warning-fg"
          // The line truncates; the title keeps the full error one
          // hover away.
          title={progress.latestRetryError}
        >
          ↻ <span className="font-mono">{retryCount}</span> {retryNoun}
          {progress.latestRetryError && (
            <span className="text-fg-subtle"> — {progress.latestRetryError}</span>
          )}
        </span>
      )}
    </p>
  );
}

// BannerStatusIcon is the running-spinner / check / cross icon
// shared between the generic BannerCard and the whats-next NodeBanner.
export function BannerStatusIcon({ status }: { status: BannerStatus }) {
  if (status === "running") {
    return (
      <ReloadIcon
        className="w-3.5 h-3.5 text-accent-text animate-spin"
        aria-label="In progress"
      />
    );
  }
  if (status === "done") {
    return (
      <CheckCircledIcon
        className="w-3.5 h-3.5 text-success-fg"
        aria-label="Completed"
      />
    );
  }
  return (
    <CrossCircledIcon
      className="w-3.5 h-3.5 text-danger-fg"
      aria-label="Failed"
    />
  );
}
