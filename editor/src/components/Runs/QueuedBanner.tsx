import { useState } from "react";

import { cancelRun, type RunHeader as RunHeaderType } from "@/api/runs";
import { Button } from "@/components/ui";

// QueuedBanner replaces the RunMetrics + Scrubber strip in RunView
// while a run is sitting on the NATS queue waiting for a runner pod.
// Carries the (optional) queue position and a cancel button so the
// user can pull the run before it ever picks up. The IR canvas
// stays mounted underneath so the workflow shape is still visible.
//
// Cloud-ready plan §F (T-15, T-31, T-32).

interface Props {
  run: RunHeaderType;
}

function ordinal(n: number): string {
  // 1 → 1st, 2 → 2nd, 3 → 3rd, 11→11th. Standard English rules.
  const mod10 = n % 10;
  const mod100 = n % 100;
  if (mod10 === 1 && mod100 !== 11) return `${n}st`;
  if (mod10 === 2 && mod100 !== 12) return `${n}nd`;
  if (mod10 === 3 && mod100 !== 13) return `${n}rd`;
  return `${n}th`;
}

export default function QueuedBanner({ run }: Props) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const onCancel = async () => {
    setBusy(true);
    setError(null);
    try {
      await cancelRun(run.id);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  // queue_position is server-computed and only present once the run
  // actually lands on the queue. While the value is racing the launch
  // POST round-trip we render a generic "in queue" copy so the banner
  // is never empty.
  const positionCopy =
    typeof run.queue_position === "number" && run.queue_position > 0
      ? `${ordinal(run.queue_position)} in queue`
      : "Waiting on the queue";

  return (
    <div className="px-4 py-2 flex items-center gap-3 border-b border-border-default bg-surface-1 text-xs">
      <span className="inline-flex items-center gap-1 text-fg-muted">
        <span aria-hidden>⧗</span>
        <span className="uppercase tracking-wide">Queued</span>
      </span>
      <span className="text-fg-default font-medium">{positionCopy}</span>
      <span className="text-fg-subtle">
        Waiting for an iterion runner to pick this run up.
      </span>
      {error && (
        <span className="text-[10px] text-danger truncate max-w-xs">
          {error}
        </span>
      )}
      <div className="ml-auto">
        <Button
          variant="danger"
          size="sm"
          onClick={() => void onCancel()}
          disabled={busy}
        >
          Cancel
        </Button>
      </div>
    </div>
  );
}
