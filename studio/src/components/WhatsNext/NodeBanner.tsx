import { useMemo, useState } from "react";

import {
  BannerProgressLine,
  BannerStatusIcon,
} from "@/components/Runs/conversation/BannerCard";
import { ThinkingIndicator } from "@/components/ui/ThinkingIndicator";
import { phrasesForNode } from "@/lib/whats-next/loadingPhrases";
import type { BannerMessage } from "@/lib/runChat/types";

interface Props {
  message: BannerMessage;
}

// NodeBanner is the whats-next-flavoured banner row: same shape as
// the generic BannerCard but with two whats-next-specific affordances
// — a per-node ThinkingIndicator (typing animation + ✻ glyph, shared
// with the Runs/logs ThinkingFooter) and a Summary <details> block
// (whats-next renders no NodeOutputCard, so the summary doubles as the
// at-a-glance result).
export default function NodeBanner({ message }: Props) {
  const { label, status, summary, errorMessage, nodeId, progress } = message;
  // Cache the per-node phrase list so it's stable across re-renders.
  const phrases = useMemo(() => phrasesForNode(nodeId), [nodeId]);

  return (
    <div className="flex items-start gap-2 text-[12px]">
      <div className="mt-0.5 shrink-0">
        <BannerStatusIcon status={status} />
      </div>
      <div className="flex-1 min-w-0">
        <div className="flex items-baseline gap-2">
          <span className="text-fg-default">{label}</span>
          <span className="text-[10px] font-mono text-fg-subtle">{nodeId}</span>
          {status === "running" && (
            <ThinkingIndicator words={phrases} active className="font-mono text-[10px] text-info-fg italic" />
          )}
        </div>
        {progress && status === "running" && (
          <BannerProgressLine progress={progress} />
        )}
        {summary && status === "done" && <BannerSummary text={summary} />}
        {errorMessage && status === "failed" && (
          <p className="mt-1 text-[11px] text-danger-fg">{errorMessage}</p>
        )}
      </div>
    </div>
  );
}

// BannerSummary shows the first ~140 chars of the node's structured
// summary inline (always visible), with a "Show more" toggle to expand
// the rest. Differentiates repeated banners (5 successive triage_board
// invocations in the same chat would otherwise all look identical with
// just a "Summary" toggle each — operator has to click every one to
// see what happened). Single-line summaries shorter than the cap render
// without the toggle at all.
function BannerSummary({ text }: { text: string }) {
  const PEEK_CHARS = 140;
  const [expanded, setExpanded] = useState(false);
  const single = text.trim();
  const needsToggle = single.length > PEEK_CHARS;
  const peek = needsToggle
    ? `${single.slice(0, PEEK_CHARS).trimEnd()}…`
    : single;
  return (
    <div className="mt-1 text-[12px] text-fg-default border-l-2 border-border-subtle pl-2">
      <p className="whitespace-pre-wrap break-words">
        {expanded ? single : peek}
      </p>
      {needsToggle && (
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          className="mt-1 text-[11px] text-fg-muted hover:text-fg-default underline-offset-2 hover:underline"
        >
          {expanded ? "Show less" : "Show more"}
        </button>
      )}
    </div>
  );
}
