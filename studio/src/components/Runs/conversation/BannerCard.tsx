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
    <div className="flex items-start gap-2 text-[12px]">
      <div className="mt-0.5 shrink-0">
        <BannerStatusIcon status={status} />
      </div>
      <div className="flex-1 min-w-0">
        <div className="flex items-baseline gap-2">
          <span className="text-fg-default">{label}</span>
          {label !== nodeId && (
            <span className="text-[10px] font-mono text-fg-subtle">{nodeId}</span>
          )}
        </div>
        {progress && status === "running" && <BannerProgressLine progress={progress} />}
        {errorMessage && status === "failed" && (
          <p className="mt-1 text-[11px] text-danger-fg">{errorMessage}</p>
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
  if (progress.toolCount === 0) return null;
  const noun = progress.toolCount === 1 ? "tool call" : "tool calls";
  return (
    <p className="mt-1 text-[11px] text-fg-muted truncate">
      <span className="font-mono">{progress.toolCount}</span> {noun}
      {progress.latestTool && (
        <>
          {" "}· latest:{" "}
          <code className="text-[11px] font-mono text-fg-default">
            {progress.latestTool}
          </code>
          {progress.latestToolHint &&
            progress.latestToolHint !== progress.latestTool && (
              <span className="text-fg-subtle"> — {progress.latestToolHint}</span>
            )}
        </>
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
        className="w-3.5 h-3.5 text-accent animate-spin"
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
