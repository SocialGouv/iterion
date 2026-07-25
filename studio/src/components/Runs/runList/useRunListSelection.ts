import { useCallback, useEffect, useMemo, useState } from "react";

import type { RunSummary } from "@/api/runs";

export interface UseRunListSelectionResult {
  selectedIds: ReadonlySet<string>;
  selectedRuns: RunSummary[];
  allSelected: boolean;
  toggle: (id: string) => void;
  toggleAll: () => void;
  clear: () => void;
}

// Multi-selection over the currently visible (filtered + sorted) run
// list. Selection is pruned whenever a selected run leaves the visible
// set (deleted, or filtered out) so bulk actions can never target a
// run the user no longer sees.
export function useRunListSelection(
  visibleRuns: RunSummary[],
): UseRunListSelectionResult {
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(
    () => new Set(),
  );

  const visibleIds = useMemo(
    () => new Set(visibleRuns.map((r) => r.id)),
    [visibleRuns],
  );

  useEffect(() => {
    setSelectedIds((prev) => {
      if (prev.size === 0) return prev;
      const next = new Set<string>();
      for (const id of prev) if (visibleIds.has(id)) next.add(id);
      return next.size === prev.size ? prev : next;
    });
  }, [visibleIds]);

  const toggle = useCallback((id: string) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const toggleAll = useCallback(() => {
    setSelectedIds((prev) =>
      prev.size === visibleIds.size && visibleIds.size > 0
        ? new Set()
        : new Set(visibleIds),
    );
  }, [visibleIds]);

  const clear = useCallback(() => setSelectedIds(new Set()), []);

  const selectedRuns = useMemo(
    () => visibleRuns.filter((r) => selectedIds.has(r.id)),
    [visibleRuns, selectedIds],
  );

  return {
    selectedIds,
    selectedRuns,
    allSelected: visibleRuns.length > 0 && selectedIds.size === visibleRuns.length,
    toggle,
    toggleAll,
    clear,
  };
}
