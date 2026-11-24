import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { getCostCapStatus, overrideCostCapForToday } from "@/api/limits";
import type { CostCapStatus } from "@/api/types";
import { Button } from "@/components/ui";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

const POLL_INTERVAL_MS = 5000;

// CostCapBanner renders a sticky warning when the per-(store, UTC-day)
// LLM spend cap is reached, with a one-click "Override for today" action.
// It polls GET /api/v1/limits/cost via TanStack Query (which pauses while
// the tab is hidden and de-dupes the two mount points — Home + Dispatcher)
// and renders nothing when the cap is disabled, under budget, or already
// overridden.
export default function CostCapBanner() {
  const enabled = useServerInfoStore((s) => s.info?.cost_cap_enabled ?? false);
  const addToast = useUIStore((s) => s.addToast);
  const [busy, setBusy] = useState(false);

  const query = useQuery<CostCapStatus>({
    queryKey: ["cost-cap-status"],
    queryFn: getCostCapStatus,
    enabled,
    refetchInterval: POLL_INTERVAL_MS,
    refetchIntervalInBackground: false,
  });
  const status = query.data;

  if (!enabled || !status || !status.enabled || !status.exceeded) {
    return null;
  }

  const onOverride = async () => {
    setBusy(true);
    try {
      await overrideCostCapForToday({ note: "override-for-today" });
      await query.refetch();
      addToast("Daily spend cap overridden for today", "info");
    } catch (e) {
      addToast(
        e instanceof Error ? e.message : "Failed to override spend cap",
        "error",
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      className="shrink-0 px-4 py-2 bg-warning-soft/60 border-b border-warning/40 flex flex-wrap items-center gap-x-3 gap-y-1 text-body"
      role="status"
      aria-live="polite"
    >
      <span className="font-mono text-warning">⚠ Daily spend cap reached</span>
      <span className="text-fg-muted">
        ${status.spent_usd.toFixed(2)} of ${status.limit_usd.toFixed(2)} spent
        today ({status.date} UTC). New runs are paused.
      </span>
      <span className="text-fg-muted">Resets automatically at the next UTC day.</span>
      <div className="ml-auto flex items-center gap-2">
        <Button
          variant="primary"
          size="sm"
          disabled={busy}
          onClick={() => void onOverride()}
        >
          {busy ? "…" : "Override for today"}
        </Button>
      </div>
    </div>
  );
}
