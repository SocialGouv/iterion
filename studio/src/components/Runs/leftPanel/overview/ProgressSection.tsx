import { useState } from "react";

import { IconButton, Meter, Stat } from "@/components/ui";
import type { RunMetrics } from "@/hooks/useRunMetrics";
import type { RunHeader } from "@/api/runs";
import { BumpLoopDialog, isRunSteerable } from "../../runSteering";

import { Section } from "../InfoPrimitives";

interface ProgressSectionProps {
  run: RunHeader;
  metrics: RunMetrics;
  onJumpToFailed?: (nodeId: string) => void;
}

// ProgressSection is the run's execution progress: the node counts
// (total / active / paused / failed) and a meter per named loop. It
// absorbs the counters that used to crowd the header's metrics strip,
// giving them room to breathe. Failed is a click-through to the first
// failed node when the parent wires a jump handler.
export function ProgressSection({
  run,
  metrics,
  onJumpToFailed,
}: ProgressSectionProps) {
  const [bumpLoopName, setBumpLoopName] = useState<string | null>(null);
  const loops = run.loops
    ? Object.entries(run.loops).sort(([a], [b]) => a.localeCompare(b))
    : [];
  const steerable = isRunSteerable(run.status);
  const hasCounts =
    metrics.nodeCount > 0 ||
    metrics.branchCountActive > 0 ||
    metrics.pausedCount > 0 ||
    metrics.failedCount > 0;

  if (!hasCounts && loops.length === 0) return null;

  const failedId = metrics.firstFailedNodeId;

  return (
    <Section title="Progress">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Stat label="nodes" value={String(metrics.nodeCount)} />
        {metrics.branchCountActive > 0 && (
          <Stat
            label="active"
            value={String(metrics.branchCountActive)}
            tone="info"
            live
          />
        )}
        {metrics.pausedCount > 0 && (
          <Stat
            label="paused"
            value={String(metrics.pausedCount)}
            tone="warning"
          />
        )}
        {metrics.failedCount > 0 &&
          (onJumpToFailed && failedId ? (
            <Stat
              label="failed"
              value={String(metrics.failedCount)}
              tone="danger"
              onClick={() => onJumpToFailed(failedId)}
              hint={`Jump to first failed node: ${failedId}`}
              ariaLabel={`failed ${metrics.failedCount}, jump to first failed node`}
            />
          ) : (
            <Stat
              label="failed"
              value={String(metrics.failedCount)}
              tone="danger"
            />
          ))}
      </div>

      {loops.length > 0 && (
        <div className="space-y-1.5 pt-1.5">
          {loops.map(([name, p]) => (
            <div key={name} className="flex items-end gap-1">
              <div className="min-w-0 flex-1">
                <Meter
                  label={`⟳ ${name}`}
                  value={p.current}
                  max={p.max || undefined}
                  fixedTone="live"
                  formatValue={(v) => String(Math.round(v))}
                  hint={`Named loop "${name}" — iteration ${p.current}${
                    p.max ? ` of ${p.max}` : " (unbounded)"
                  }.`}
                />
              </div>
              {steerable && (p.max ?? 0) > 0 && (
                <IconButton
                  size="sm"
                  label={`Grant extra iterations to loop ${name}`}
                  onClick={() => setBumpLoopName(name)}
                >
                  +
                </IconButton>
              )}
            </div>
          ))}
        </div>
      )}
      {bumpLoopName != null && (
        <BumpLoopDialog
          open
          onOpenChange={(open) => {
            if (!open) setBumpLoopName(null);
          }}
          runId={run.id}
          loopName={bumpLoopName}
          currentMax={run.loops?.[bumpLoopName]?.max || undefined}
        />
      )}
    </Section>
  );
}
