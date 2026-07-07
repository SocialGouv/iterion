import { useMemo } from "react";

import { EmptyState } from "@/components/ui";
import { useNow } from "@/hooks/useNow";
import { useRunMetrics } from "@/hooks/useRunMetrics";
import { useRunStore } from "@/store/run";
import type { RunHeader } from "@/api/runs";

import { StatusHero } from "./overview/StatusHero";
import { BudgetSection } from "./overview/BudgetSection";
import { ProgressSection } from "./overview/ProgressSection";
import { BriefingSection } from "./overview/BriefingSection";
import { ConfigurationSection } from "./overview/ConfigurationSection";
import { OutcomeSection } from "./overview/OutcomeSection";
import { AdvancedSection } from "./overview/AdvancedSection";

interface OverviewPanelProps {
  runId: string;
  run: RunHeader | null;
  // Switches the left panel to a sibling tab — wired from the Outcome
  // section's "N files / N commits" jumps.
  onSwitchTab: (tab: "files" | "commits") => void;
  // Selects the first failed node on the canvas — wired to the Progress
  // section's "failed" chip.
  onJumpToFailed?: (nodeId: string) => void;
}

// OverviewPanel is the run console's mission control: a scannable stack
// that answers, top to bottom, "how's it going right now?" (StatusHero),
// "what's it costing vs its budget?" (Budget), "how far along?"
// (Progress), "what was it asked to do?" (Briefing), "how was it
// configured?" (Configuration), "where did the work land?" (Outcome),
// and the raw identity/timing detail on demand (Advanced — the folded-in
// former Info tab). Runtime figures come from one shared useRunMetrics so
// the per-second duration tick refolds the events stream only once.
export default function OverviewPanel({
  runId,
  run,
  onSwitchTab,
  onJumpToFailed,
}: OverviewPanelProps) {
  const executionsById = useRunStore((s) => s.executionsById);
  const executions = useMemo(
    () => Array.from(executionsById.values()),
    [executionsById],
  );
  const active = run?.status === "running";
  const nowMs = useNow(active ? 1000 : null);
  const metrics = useRunMetrics(nowMs);

  if (!run) {
    return <EmptyState message="Loading…" />;
  }

  return (
    <div className="flex flex-col min-h-0 min-w-0 flex-1 w-full overflow-y-auto">
      <div className="px-3 py-2 space-y-3">
        <StatusHero
          run={run}
          executions={executions}
          nowMs={nowMs}
          durationMs={metrics.durationMs}
        />
        <BudgetSection run={run} metrics={metrics} />
        <ProgressSection
          run={run}
          metrics={metrics}
          onJumpToFailed={onJumpToFailed}
        />
        <BriefingSection run={run} />
        <ConfigurationSection run={run} />
        <OutcomeSection runId={runId} run={run} onSwitchTab={onSwitchTab} />
        <AdvancedSection run={run} />
      </div>
    </div>
  );
}
