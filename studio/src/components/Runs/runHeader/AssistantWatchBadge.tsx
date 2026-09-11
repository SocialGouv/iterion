import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  listAssistantRunWatches,
  stopAssistantRunWatch,
} from "@/api/runs";
import { Tooltip } from "@/components/ui";

export default function AssistantWatchBadge({ runId }: { runId: string }) {
  const queryClient = useQueryClient();
  const queryKey = ["assistant-run-watches", runId] as const;
  const watches = useQuery({
    queryKey,
    queryFn: () => listAssistantRunWatches(runId),
    refetchInterval: 20_000,
  });
  const stop = useMutation({
    mutationFn: stopAssistantRunWatch,
    onSuccess: () => queryClient.invalidateQueries({ queryKey }),
  });
  const active = watches.data ?? [];
  if (active.length === 0) return null;
  const watch = active[0];
  if (!watch) return null;
  return (
    <Tooltip
      content={`The linked assistant will wake at its safe chat boundary if this run fails (${watch.mode} mode). Local watches require the Studio process to remain running.`}
    >
      <span className="inline-flex items-center gap-1 rounded border border-accent/40 bg-accent-soft px-1.5 py-0.5 text-micro font-medium text-accent-text">
        <span aria-hidden>◉</span>
        Assistant watch
        <button
          type="button"
          className="ml-0.5 opacity-70 hover:opacity-100"
          aria-label="Stop assistant watch"
          disabled={stop.isPending}
          onClick={() => stop.mutate(watch.id)}
        >
          ×
        </button>
      </span>
    </Tooltip>
  );
}
