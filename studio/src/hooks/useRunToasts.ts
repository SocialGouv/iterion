import { useEffect, useRef } from "react";

import type { RunEvent } from "@/api/runs";
import { useUIStore } from "@/store/ui";

// useRunToasts surfaces run-level milestones as transient toasts. The
// high-water mark is seeded from `snapshotLastSeq` so historical
// events (events already covered by the snapshot the user landed on,
// or fetched lazily via /events for the EventLog / Scrubber replay)
// don't dump a burst of past milestone toasts on screen. Only events
// with seq > snapshot.last_seq fire — i.e., genuinely live updates
// the user hasn't seen before. Re-anchors when snapshotLastSeq
// changes (navigating to a different run).
export function useRunToasts(
  events: RunEvent[],
  snapshotLastSeq: number | null | undefined,
): void {
  const addToast = useUIStore((s) => s.addToast);
  const anchorRef = useRef<number | null>(null);
  const anchoredAtRef = useRef<number | null | undefined>(undefined);

  useEffect(() => {
    if (anchoredAtRef.current !== snapshotLastSeq) {
      anchoredAtRef.current = snapshotLastSeq;
      anchorRef.current = snapshotLastSeq ?? null;
    }
    if (anchorRef.current === null) return;
    for (const e of events) {
      if (e.seq <= anchorRef.current) continue;
      const toast = toastForEvent(e);
      if (toast) addToast(toast.message, toast.type);
      anchorRef.current = e.seq;
    }
  }, [events, snapshotLastSeq, addToast]);
}

export function toastForEvent(
  e: RunEvent,
): { message: string; type: "success" | "error" | "info" | "warning" } | null {
  switch (e.type) {
    case "run_finished":
      return { message: "Run finished", type: "success" };
    case "run_failed": {
      const err = (e.data?.["error"] as string | undefined) ?? "see logs";
      return { message: `Run failed: ${err}`, type: "error" };
    }
    case "run_paused": {
      // Operator-initiated pauses are non-urgent — they happen because
      // the operator clicked Pause, so a warning toast over-dramatizes
      // the event. Engine-side input-requested pauses keep the
      // warning tone so they read as action-required.
      const reason = e.data?.["reason"] as string | undefined;
      if (reason === "operator") {
        return { message: "Run paused by operator", type: "info" };
      }
      return { message: "Run paused — input requested", type: "warning" };
    }
    case "run_resumed":
      return { message: "Run resumed", type: "info" };
    case "run_cancelled":
      return { message: "Run cancelled", type: "info" };
    case "budget_exceeded": {
      const dim = (e.data?.["dimension"] as string | undefined) ?? "budget";
      return { message: `Budget exhausted: ${dim} hit hard cap.`, type: "error" };
    }
    default:
      return null;
  }
}
