import { useEffect, useMemo, useRef } from "react";
import { useUIStore } from "@/store/ui";
import { useGroupedDiagnostics } from "@/hooks/useGroupedDiagnostics";

/**
 * Edge-triggered: opens the diagnostics panel when the error count
 * transitions from 0 → ≥1 *during* the session. Errors already present
 * at mount don't auto-open the panel — the user opted into a closed
 * default and shouldn't be surprised on cold load. The next clean →
 * dirty transition re-arms the trigger.
 *
 * Warnings alone do NOT trigger auto-open: the panel is reserved for
 * blocking errors so non-fatal advice doesn't steal canvas real estate.
 */
export function useAutoOpenDiagnosticsOnError(active = true): void {
  const grouped = useGroupedDiagnostics();
  const openDiagnosticsPanel = useUIStore((s) => s.openDiagnosticsPanel);

  const errorCount = useMemo(
    () => grouped.all.reduce((acc, d) => acc + (d.severity === "error" ? 1 : 0), 0),
    [grouped],
  );

  const prevErrorCountRef = useRef(errorCount);

  useEffect(() => {
    // The panel is app-wide: a tab behind another does not open it over the
    // one on screen. Its edge waits — read against the count it last saw on
    // screen — so a tab that turned red while hidden opens it when shown.
    if (!active) return;
    const prev = prevErrorCountRef.current;
    if (prev === 0 && errorCount > 0) {
      openDiagnosticsPanel();
    }
    prevErrorCountRef.current = errorCount;
  }, [active, errorCount, openDiagnosticsPanel]);
}
