import { useEffect, useMemo, useState } from "react";

import {
  BannerProgressLine,
  BannerStatusIcon,
} from "@/components/Runs/conversation/BannerCard";
import { phrasesForNode } from "@/lib/whats-next/loadingPhrases";
import type { BannerMessage } from "@/lib/runChat/types";

interface Props {
  message: BannerMessage;
}

// NodeBanner is the whats-next-flavoured banner row: same shape as
// the generic BannerCard but with two whats-next-specific affordances
// — a per-node LoadingPhrase rotator (whimsy) and a Summary <details>
// block (whats-next renders no NodeOutputCard, so the summary doubles
// as the at-a-glance result).
export default function NodeBanner({ message }: Props) {
  const { label, status, summary, errorMessage, nodeId, progress } = message;

  return (
    <div className="flex items-start gap-2 text-[12px]">
      <div className="mt-0.5 shrink-0">
        <BannerStatusIcon status={status} />
      </div>
      <div className="flex-1 min-w-0">
        <div className="flex items-baseline gap-2">
          <span className="text-fg-default">{label}</span>
          <span className="text-[10px] font-mono text-fg-subtle">{nodeId}</span>
          {status === "running" && <LoadingPhrase nodeId={nodeId} />}
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

function LoadingPhrase({ nodeId }: { nodeId: string }) {
  const phrases = useMemo(() => phrasesForNode(nodeId), [nodeId]);
  // Random starting index so two adjacent banners (rare, but possible
  // on parallel branches) don't lock-step through the same phrases.
  const [index, setIndex] = useState(() =>
    phrases.length > 0 ? Math.floor(Math.random() * phrases.length) : 0,
  );
  useEffect(() => {
    if (phrases.length <= 1) return;
    const id = setInterval(() => {
      setIndex((i) => (i + 1) % phrases.length);
    }, 2500);
    return () => clearInterval(id);
  }, [phrases.length]);
  if (phrases.length === 0) return null;
  return (
    <span className="text-[10px] text-fg-subtle italic">
      {phrases[index]}…
    </span>
  );
}
