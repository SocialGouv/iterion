import { CheckCircledIcon, ReloadIcon, CrossCircledIcon } from "@radix-ui/react-icons";

import type { BannerMessage } from "@/lib/pilote/messages";

interface Props {
  message: BannerMessage;
}

export default function NodeBanner({ message }: Props) {
  const { label, status, summary, errorMessage, nodeId, progress } = message;

  return (
    <div className="flex items-start gap-2 text-[12px]">
      <div className="mt-0.5 shrink-0">{statusIcon(status)}</div>
      <div className="flex-1 min-w-0">
        <div className="flex items-baseline gap-2">
          <span className="text-fg-default">{label}</span>
          <span className="text-[10px] font-mono text-fg-subtle">{nodeId}</span>
          {status === "running" && (
            <span className="text-[10px] text-fg-subtle">…</span>
          )}
        </div>
        {progress && status === "running" && (
          <ProgressLine progress={progress} />
        )}
        {summary && status === "done" && (
          <details className="mt-1 group">
            <summary className="cursor-pointer text-[11px] text-fg-muted hover:text-fg-default select-none">
              Summary
            </summary>
            <p className="mt-1 text-[12px] whitespace-pre-wrap text-fg-default border-l-2 border-border-subtle pl-2">
              {summary}
            </p>
          </details>
        )}
        {errorMessage && status === "failed" && (
          <p className="mt-1 text-[11px] text-danger-fg">{errorMessage}</p>
        )}
      </div>
    </div>
  );
}

function ProgressLine({
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
          {progress.latestToolHint && progress.latestToolHint !== progress.latestTool && (
            <span className="text-fg-subtle"> — {progress.latestToolHint}</span>
          )}
        </>
      )}
    </p>
  );
}

function statusIcon(status: BannerMessage["status"]) {
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
