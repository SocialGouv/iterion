// Wire protocol for the run WebSocket: the envelope shape and the
// builders for every message the client sends. Pure — the hook
// stringifies and sends. Do NOT reorder object keys or change payload
// shapes: the serialized JSON is the wire contract with
// pkg/server/runs_ws.go.

import type { RunEvent } from "@/api/runs";

export interface WsEnvelope {
  type: string;
  payload?: unknown;
  ack_id?: string;
}

export interface LogChunkPayload {
  offset: number;
  text: string;
  total?: number;
}

// Subscribe from the highest seq the store has actually consumed.
// We can't use snapshot.last_seq alone: the REST `getRun` call in
// RunView seeds the snapshot before any events arrive, so an
// events-less store with last_seq=N would otherwise request
// from_seq=N+1 and miss the entire history (the bug that hid
// edges on finished runs).
//
// replay_history is true ONLY when we already have events
// locally (i.e., this is a reconnect after an outage): the
// server fills the gap between FromSeq and snapshotSeq. On
// initial connect (empty store) we run lazy — snapshot only,
// then live tail — and let loadEventHistoryIfMissing pull
// the historical events via HTTP if and when something needs
// them. Eliminates the 30s replay-stall on first open.
export function subscribeEnvelope(events: readonly RunEvent[]): WsEnvelope {
  const fromSeq = events.length > 0 ? events[events.length - 1]!.seq + 1 : 0;
  return {
    type: "subscribe",
    payload: {
      from_seq: fromSeq,
      replay_history: events.length > 0,
    },
  };
}

// The onopen re-subscribe path: omits the payload entirely at offset 0
// (historical shape — the server's zero-value unmarshal handles it).
export function resubscribeLogsEnvelope(fromOffset: number): WsEnvelope {
  return {
    type: "subscribe_logs",
    payload: fromOffset > 0 ? { from_offset: fromOffset } : undefined,
  };
}

// The imperative subscribeLogs path: ALWAYS sends from_offset (even
// when 0). The server's payload unmarshal short-circuits when payload
// is empty; the path worked fine for offset=0 because FromOffset's
// zero value is 0, but being explicit removes ambiguity and matches
// the shape of every other subscribe_logs message we send (the onopen
// reconnect path also sends explicit offsets > 0).
export function subscribeLogsEnvelope(fromOffset: number): WsEnvelope {
  return {
    type: "subscribe_logs",
    payload: { from_offset: fromOffset },
  };
}

export function unsubscribeLogsEnvelope(): WsEnvelope {
  return { type: "unsubscribe_logs" };
}

export function unsubscribeEnvelope(): WsEnvelope {
  return { type: "unsubscribe" };
}

export function pingEnvelope(): WsEnvelope {
  return { type: "ping" };
}
