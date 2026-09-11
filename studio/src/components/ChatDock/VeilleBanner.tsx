// The standby chip: what the assistant is watching, and the one control that
// stops it.
//
// It does NOT replace the composer. The pause has to stay open for a host
// event to be deliverable at all, and writing during a standby does not break
// it — a watch is only stopped when the assistant run itself ends. Hiding the
// input would make a reachable assistant look unreachable, and would remove
// the operator's way to redirect a standby ("watch that one instead").

import { AlertCircle, Eye, Loader2 } from "lucide-react";

export interface VeilleBannerProps {
  issueIds: string[];
  runTargets: string[];
  busy: boolean;
  error: string | null;
  onStop: () => void;
  assistantLabel: string;
  // A watch can stay active while the assistant itself is working.  Do not
  // present that state as a passive standby: it makes an active conversation
  // look abandoned.
  isRunning?: boolean;
}

// subjectLabel names what is being watched in the operator's own terms. The
// card is the subject they chose; a bare run target only appears for a watch
// posted directly on a run.
export function subjectLabel(issueIds: string[], runTargets: string[]): string {
  const cards = issueIds.length;
  const runs = runTargets.length;
  const parts: string[] = [];
  if (cards > 0) parts.push(cards === 1 ? "1 card" : `${cards} cards`);
  if (runs > 0) parts.push(runs === 1 ? "1 run tree" : `${runs} run trees`);
  return parts.join(" and ");
}

// stopLabel spells out how many channels the button cuts, because the two are
// stopped through different calls and an operator who sees only "stop" has no
// way to know a card subscription would otherwise re-arm a fresh run watch.
export function stopLabel(issueIds: string[], runTargets: string[]): string {
  return issueIds.length > 0 && runTargets.length > 0
    ? "Stop watching (card and run)"
    : "Stop watching";
}

export default function VeilleBanner({
  issueIds,
  runTargets,
  busy,
  error,
  onStop,
  assistantLabel,
  isRunning = false,
}: VeilleBannerProps) {
  return (
    <div className="border-t border-default bg-subtle px-3 py-2">
      <div className="flex items-center gap-2">
        {isRunning ? (
          <Loader2
            className="size-4 shrink-0 animate-spin text-muted-fg"
            aria-hidden
          />
        ) : (
          <Eye className="size-4 shrink-0 text-muted-fg" aria-hidden />
        )}
        <p
          className="min-w-0 flex-1 text-caption text-muted-fg"
          role={isRunning ? "status" : undefined}
          aria-live={isRunning ? "polite" : undefined}
        >
          {isRunning ? (
            <>
              {assistantLabel} is working on your request while watching{" "}
              {subjectLabel(issueIds, runTargets)}.
            </>
          ) : (
            <>
              {assistantLabel} is standing by on{" "}
              {subjectLabel(issueIds, runTargets)} — you will hear back when
              an actionable outcome needs attention.
            </>
          )}
        </p>
        <button
          type="button"
          className="shrink-0 rounded border border-default px-2 py-1 text-caption hover:bg-default disabled:opacity-50"
          disabled={busy}
          onClick={onStop}
        >
          {busy ? (
            <Loader2 className="size-3.5 animate-spin" aria-hidden />
          ) : (
            stopLabel(issueIds, runTargets)
          )}
        </button>
      </div>
      {error && (
        <p className="mt-1 flex items-center gap-1 text-caption text-danger-fg">
          <AlertCircle className="size-3.5 shrink-0" aria-hidden />
          {error}
        </p>
      )}
    </div>
  );
}
