// Inbound message routing for the run WebSocket. Pure dispatch from a
// parsed envelope onto a sink interface — the hook wires the sinks to
// the run store / heartbeat / alert plumbing. asRunEvent is the
// typed-ingress boundary: raw envelope JSON becomes a RunEvent here,
// and a malformed payload THROWS to the caller (which owns the
// drop-and-log policy for the whole parse+route step).

import { asRunEvent, type RunEvent, type RunSnapshot } from "@/api/runs";

import type { LogChunkPayload, WsEnvelope } from "./protocol";

export interface RunWsMessageSinks {
  applySnapshot: (snap: RunSnapshot) => void;
  // Live-tail single event → microtask-coalesced store push.
  queueEvent: (evt: RunEvent) => void;
  // Drain the coalescing buffer synchronously (seq-order fence before
  // snapshot swaps and bulk batches).
  flushEvents: () => void;
  applyEventsBatch: (events: RunEvent[]) => void;
  applyLogChunk: (chunk: LogChunkPayload) => void;
  markLogTerminated: () => void;
  // Run-health alert events (pkg/store.EventAlert) are in-process-only:
  // never persisted, fanned out without a seq — they must stay out of
  // the seq-ordered event store entirely (see the hook's alert notes).
  onAlertEvent: (evt: RunEvent) => void;
  // The run reached a terminal status; the broker has closed the
  // channel. The hook stops the heartbeat + marks the WS closed.
  onTerminated: () => void;
}

export function routeRunWsMessage(
  env: WsEnvelope,
  sinks: RunWsMessageSinks,
): void {
  switch (env.type) {
    case "snapshot":
      // Drain any queued events before swapping the snapshot
      // so they aren't applied against the new (empty) base.
      sinks.flushEvents();
      sinks.applySnapshot(env.payload as RunSnapshot);
      break;
    case "event": {
      const evt = asRunEvent(env.payload);
      if (evt.type === "alert") {
        sinks.onAlertEvent(evt);
        break;
      }
      sinks.queueEvent(evt);
      break;
    }
    case "event_batch": {
      // Server-side bulk envelope (replay path): payload is
      // already an array. Drain the live-event microtask
      // buffer first so seq order is preserved across
      // batches, then push the whole array in one shot —
      // bypasses the per-event microtask round-trip. Alert
      // events are never persisted so they don't appear in a
      // replay batch, but partition defensively in case a sink
      // ever multiplexes one in.
      sinks.flushEvents();
      const batch = Array.isArray(env.payload)
        ? env.payload.map(asRunEvent)
        : [];
      const persisted: RunEvent[] = [];
      for (const e of batch) {
        if (e.type === "alert") sinks.onAlertEvent(e);
        else persisted.push(e);
      }
      if (persisted.length > 0) sinks.applyEventsBatch(persisted);
      break;
    }
    case "log_chunk":
      sinks.applyLogChunk(env.payload as LogChunkPayload);
      break;
    case "log_terminated":
      sinks.markLogTerminated();
      break;
    case "pong":
      // Heartbeat reply — liveness is recorded by the caller before
      // routing.
      break;
    case "terminated":
      sinks.onTerminated();
      break;
    case "error":
      // Surface but don't tear down — a single bad command
      // shouldn't kill the live stream.
      console.warn("run ws error:", env.payload);
      break;
    case "ack":
      // No-op for now; the UI doesn't track ack_ids yet.
      break;
    default:
      break;
  }
}
