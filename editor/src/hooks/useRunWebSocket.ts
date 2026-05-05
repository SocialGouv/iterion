import { useEffect, useRef } from "react";

import type { RunEvent, RunSnapshot } from "@/api/runs";
import { useRunStore } from "@/store/run";

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

interface WsEnvelope {
  type: string;
  payload?: unknown;
  ack_id?: string;
}

function deriveWsUrl(runId: string): string {
  if (BASE_URL.startsWith("http")) {
    return BASE_URL.replace(/^http/, "ws") + `/ws/runs/${encodeURIComponent(runId)}`;
  }
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${BASE_URL}/ws/runs/${encodeURIComponent(runId)}`;
}

interface LogChunkPayload {
  offset: number;
  text: string;
  total?: number;
}

/** Imperative handle returned by useRunWebSocket — call send() for cancel
 *  and answer commands; the connection lifecycle is managed by the hook.
 *  The log helpers are opt-in: the panel that wants live log output calls
 *  subscribeLogs() once on mount and unsubscribeLogs() on unmount. */
export interface RunWsHandle {
  send: (env: WsEnvelope) => void;
  subscribeLogs: (fromOffset?: number) => void;
  unsubscribeLogs: () => void;
}

/**
 * Subscribe to /api/ws/runs/{runId} and feed the run store. Reconnects
 * on disconnect with exponential backoff (1s → 30s) and resumes from
 * the last seen seq via subscribe{from_seq}, so missed events are
 * replayed before the live tail resumes.
 */
export function useRunWebSocket(runId: string | null): RunWsHandle {
  const wsRef = useRef<WebSocket | null>(null);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const reconnectDelay = useRef(1000);
  const aliveRef = useRef(false);
  // Track whether we asked for log streaming on this connection so a
  // reconnect can re-subscribe automatically — symmetric with the
  // event from_seq replay below. Reset on runId change.
  const logsRequestedRef = useRef(false);
  // Bump from the store after Resume/Cancel HTTP actions to redial the
  // WS — the broker drops subscribers on terminal status, so the only
  // way the resumed run reaches this client is a fresh subscribe.
  const reconnectToken = useRunStore((s) => s.wsReconnectToken);

  useEffect(() => {
    if (!runId) return;
    aliveRef.current = true;
    reconnectDelay.current = 1000;
    logsRequestedRef.current = false;

    const setWsState = useRunStore.getState().setWsState;
    const applySnapshot = useRunStore.getState().applySnapshot;
    const applyEventsBatch = useRunStore.getState().applyEventsBatch;
    const applyLogChunk = useRunStore.getState().applyLogChunk;
    const markLogTerminated = useRunStore.getState().markLogTerminated;
    const setLogSubscribed = useRunStore.getState().setLogSubscribed;

    // Coalesce events that arrive in the same microtask before pushing
    // them to the store. Replay (from_seq=0) on a long run can dump
    // hundreds–thousands of envelopes onto the message queue back-to-back;
    // committing them one-by-one through `applyEvent` triggered an
    // O(N²) array spread plus a re-render per event. Batching turns
    // that into a single state mutation per JS task.
    const eventBuffer: RunEvent[] = [];
    let flushScheduled = false;
    const flushEvents = () => {
      flushScheduled = false;
      if (eventBuffer.length === 0) return;
      const drained = eventBuffer.splice(0, eventBuffer.length);
      applyEventsBatch(drained);
    };
    const queueEvent = (evt: RunEvent) => {
      eventBuffer.push(evt);
      if (!flushScheduled) {
        flushScheduled = true;
        queueMicrotask(flushEvents);
      }
    };

    const connect = () => {
      if (!aliveRef.current) return;
      setWsState("connecting");
      const ws = new WebSocket(deriveWsUrl(runId));
      wsRef.current = ws;

      ws.onopen = () => {
        setWsState("open");
        reconnectDelay.current = 1000;

        // Resume from the highest seq the store has actually consumed.
        // We can't use snapshot.last_seq alone: the REST `getRun` call in
        // RunView seeds the snapshot before any events arrive, so an
        // events-less store with last_seq=N would otherwise request
        // from_seq=N+1 and miss the entire history (the bug that hid
        // edges on finished runs).
        const events = useRunStore.getState().events;
        const fromSeq =
          events.length > 0 ? events[events.length - 1]!.seq + 1 : 0;
        ws.send(
          JSON.stringify({
            type: "subscribe",
            payload: fromSeq > 0 ? { from_seq: fromSeq } : undefined,
          } satisfies WsEnvelope),
        );

        // Re-subscribe to logs if the user had opened the Logs tab
        // before the disconnect. We resume from the byte after our
        // last known position so the backend snapshot fills any gap
        // that landed during the outage.
        if (logsRequestedRef.current) {
          const log = useRunStore.getState().log;
          const fromOffset = log.start + log.text.length;
          ws.send(
            JSON.stringify({
              type: "subscribe_logs",
              payload: fromOffset > 0 ? { from_offset: fromOffset } : undefined,
            } satisfies WsEnvelope),
          );
          setLogSubscribed(true);
        }
      };

      ws.onmessage = (msgEv) => {
        try {
          const env = JSON.parse(msgEv.data) as WsEnvelope;
          switch (env.type) {
            case "snapshot":
              // Drain any queued events before swapping the snapshot
              // so they aren't applied against the new (empty) base.
              flushEvents();
              applySnapshot(env.payload as RunSnapshot);
              break;
            case "event":
              queueEvent(env.payload as RunEvent);
              break;
            case "log_chunk":
              applyLogChunk(env.payload as LogChunkPayload);
              break;
            case "log_terminated":
              markLogTerminated();
              break;
            case "terminated":
              // The run reached a terminal status; the broker has
              // closed the channel. We keep the socket open; the
              // server-side will eventually close it too.
              setWsState("closed");
              break;
            case "error":
              // Surface but don't tear down — a single bad command
              // shouldn't kill the live stream.
              // eslint-disable-next-line no-console
              console.warn("run ws error:", env.payload);
              break;
            case "ack":
              // No-op for now; the UI doesn't track ack_ids yet.
              break;
            default:
              break;
          }
        } catch {
          // ignore malformed messages
        }
      };

      ws.onclose = () => {
        wsRef.current = null;
        if (!aliveRef.current) {
          setWsState("closed");
          return;
        }
        setWsState("reconnecting");
        scheduleReconnect();
      };

      ws.onerror = () => {
        ws.close();
      };
    };

    const scheduleReconnect = () => {
      if (!aliveRef.current) return;
      reconnectTimer.current = setTimeout(() => {
        reconnectDelay.current = Math.min(reconnectDelay.current * 2, 30_000);
        connect();
      }, reconnectDelay.current);
    };

    connect();

    return () => {
      aliveRef.current = false;
      // Drain any events buffered for the next microtask so we don't
      // lose them when React unmounts the hook before the flush fires.
      flushEvents();
      if (reconnectTimer.current) {
        clearTimeout(reconnectTimer.current);
        reconnectTimer.current = null;
      }
      const ws = wsRef.current;
      if (ws) {
        try {
          ws.send(JSON.stringify({ type: "unsubscribe" } satisfies WsEnvelope));
        } catch {
          // ignore — the socket may already be closed
        }
        ws.close();
        wsRef.current = null;
      }
      useRunStore.getState().setWsState("closed");
    };
  }, [runId, reconnectToken]);

  return {
    send: (env) => {
      const ws = wsRef.current;
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(env));
      }
    },
    subscribeLogs: (fromOffset) => {
      logsRequestedRef.current = true;
      const ws = wsRef.current;
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      const offset =
        typeof fromOffset === "number"
          ? fromOffset
          : (() => {
              const log = useRunStore.getState().log;
              return log.start + log.text.length;
            })();
      ws.send(
        JSON.stringify({
          type: "subscribe_logs",
          payload: offset > 0 ? { from_offset: offset } : undefined,
        } satisfies WsEnvelope),
      );
      useRunStore.getState().setLogSubscribed(true);
    },
    unsubscribeLogs: () => {
      logsRequestedRef.current = false;
      const ws = wsRef.current;
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(
          JSON.stringify({ type: "unsubscribe_logs" } satisfies WsEnvelope),
        );
      }
      useRunStore.getState().setLogSubscribed(false);
    },
  };
}
