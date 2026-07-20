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
    // NOTE: run-health `alert` events are NOT handled here. They are
    // unpersisted, seq-less (Seq=0) broker events that never enter the
    // seq-ordered event store (they'd be dropped by reduceEvents and
    // corrupt the WS from_seq math). useRunWebSocket intercepts them at
    // ingestion and drives the toast + notification dot out-of-band.
  }, [events, snapshotLastSeq, addToast]);
}

export function toastForEvent(
  e: RunEvent,
): { message: string; type: "success" | "error" | "info" | "warning" } | null {
  switch (e.type) {
    case "run_finished":
      return { message: "Run finished", type: "success" };
    case "run_failed": {
      const err = e.data?.error ?? "see logs";
      return { message: `Run failed: ${err}`, type: "error" };
    }
    case "run_paused": {
      // Operator-initiated pauses are non-urgent — they happen because
      // the operator clicked Pause, so a warning toast over-dramatizes
      // the event. Engine-side input-requested pauses keep the
      // warning tone so they read as action-required.
      const reason = e.data?.reason;
      if (reason === "operator") {
        return { message: "Run paused by operator", type: "info" };
      }
      return { message: "Run paused — input requested", type: "warning" };
    }
    case "run_resumed":
      return { message: "Run resumed", type: "info" };
    case "run_cancelled":
      return { message: "Run cancelled", type: "info" };
    case "budget_warning": {
      const dim = e.data?.dimension ?? "budget";
      const used = e.data?.used;
      const limit = e.data?.limit;
      const pct =
        typeof used === "number" && typeof limit === "number" && limit > 0
          ? Math.round((used / limit) * 100)
          : null;
      const suffix = pct !== null ? ` at ${pct}%` : "";
      return { message: `Budget warning: ${dim}${suffix}.`, type: "warning" };
    }
    case "budget_exceeded": {
      const dim = e.data?.dimension ?? "budget";
      return { message: `Budget exhausted: ${dim} hit hard cap.`, type: "error" };
    }
    case "alert": {
      // In-process run-health alert (stall / budget / failure) fanned
      // out by pkg/alert's browser sink. The payload carries a
      // pre-rendered title + reason; pick the toast tone from kind.
      const kind = e.data?.kind;
      const title = e.data?.title ?? "Run alert";
      const reason = e.data?.reason;
      const message = reason ? `${title}: ${reason}` : title;
      const type: "error" | "warning" =
        kind === "budget_exceeded" || kind === "run_failed" ? "error" : "warning";
      return { message, type };
    }
    default:
      return null;
  }
}
