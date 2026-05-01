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

/** Imperative handle returned by useRunWebSocket — call send() for cancel
 *  and answer commands; the connection lifecycle is managed by the hook. */
export interface RunWsHandle {
  send: (env: WsEnvelope) => void;
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

  useEffect(() => {
    if (!runId) return;
    aliveRef.current = true;
    reconnectDelay.current = 1000;

    const setWsState = useRunStore.getState().setWsState;
    const applySnapshot = useRunStore.getState().applySnapshot;
    const applyEvent = useRunStore.getState().applyEvent;

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
      };

      ws.onmessage = (msgEv) => {
        try {
          const env = JSON.parse(msgEv.data) as WsEnvelope;
          switch (env.type) {
            case "snapshot":
              applySnapshot(env.payload as RunSnapshot);
              break;
            case "event":
              applyEvent(env.payload as RunEvent);
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
  }, [runId]);

  return {
    send: (env) => {
      const ws = wsRef.current;
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(env));
      }
    },
  };
}
