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
        {summary && status === "done" && (
          <details className="mt-1 group">
            <summary className="cursor-pointer text-[11px] text-fg-muted hover:text-fg-default select-none">
              Summary
            </summary>
            <p className="mt-1 text-[12px] whitespace-pre-wrap break-words text-fg-default border-l-2 border-border-subtle pl-2">
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
