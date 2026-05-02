import { useEffect, useRef } from "react";

import type { RunEvent } from "@/api/runs";
import { useUIStore } from "@/store/ui";

// useRunToasts surfaces run-level milestones as transient toasts. Only
// fires for events that came in since the previous render, tracked via
// a high-water-mark seq. Resets when the events array contracts (new
// run, snapshot replay) so a fresh subscription doesn't double-toast.
export function useRunToasts(events: RunEvent[]): void {
  const addToast = useUIStore((s) => s.addToast);
  const lastSeenSeqRef = useRef<number>(-1);

  useEffect(() => {
    const tail = events[events.length - 1];
    if (!tail) {
      lastSeenSeqRef.current = -1;
      return;
    }
    // The events array can shrink when the WS hook re-applies a fresh
    // snapshot; rewind the high-water-mark accordingly so we don't
    // skip toasts for the new run.
    if (tail.seq < lastSeenSeqRef.current) {
      lastSeenSeqRef.current = -1;
    }
    for (const e of events) {
      if (e.seq <= lastSeenSeqRef.current) continue;
      const toast = toastForEvent(e);
      if (toast) addToast(toast.message, toast.type);
    }
    lastSeenSeqRef.current = tail.seq;
  }, [events, addToast]);
}

function toastForEvent(
  e: RunEvent,
): { message: string; type: "success" | "error" | "info" | "warning" } | null {
  switch (e.type) {
    case "run_finished":
      return { message: "Run finished", type: "success" };
    case "run_failed": {
      const err = (e.data?.["error"] as string | undefined) ?? "see logs";
      return { message: `Run failed: ${err}`, type: "error" };
    }
    case "run_paused":
      return { message: "Run paused — input requested", type: "warning" };
    case "run_resumed":
      return { message: "Run resumed", type: "info" };
    case "run_cancelled":
      return { message: "Run cancelled", type: "info" };
    case "budget_exceeded": {
      const dim = (e.data?.["dimension"] as string | undefined) ?? "budget";
      return { message: `Budget exceeded: ${dim}`, type: "error" };
    }
    default:
      return null;
  }
}
