import { useEffect, useRef } from "react";

import type { RunEvent, RunSnapshot } from "@/api/runs";
import { getDesktopWsBase, isDesktop, isWailsHosted } from "@/lib/desktopBridge";
import { useRunStore } from "@/store/run";

const BASE_URL = import.meta.env.VITE_API_URL ?? "/api";

interface WsEnvelope {
  type: string;
  payload?: unknown;
  ack_id?: string;
}

// readStoreOverrideFromURL returns the current page's `?store=` query
// param, if any. Same helper as in api/runs.ts but inlined to avoid an
// import cycle (hooks → api → hooks would not actually cycle, but
// keeping it local minimises coupling). The WS URL must carry the
// override so the daemon's WS handler routes via resolveCrossStore
// AND streams events from the foreign store (pkg/server/runs_ws.go's
// streamEventsCrossStore path).
function readStoreOverrideFromURL(): string {
  if (typeof window === "undefined") return "";
  try {
    const v = new URLSearchParams(window.location.search).get("store");
    // Defence-in-depth: validate the shape before forwarding to the
    // daemon WS. Server still does its own check via resolveCrossStore,
    // but a malformed value should be dropped client-side too. Mirrors
    // api/runs.ts:isSafeStoreParam.
    if (!v || v.length === 0 || v.length > 512) return "";
    if (!/^[A-Za-z0-9_./-]+$/.test(v)) return "";
    if (v.includes("..")) return "";
    return v;
  } catch {
    return "";
  }
}

function appendStoreParam(wsURL: string): string {
  const override = readStoreOverrideFromURL();
  if (!override) return wsURL;
  const sep = wsURL.includes("?") ? "&" : "?";
  return `${wsURL}${sep}store=${encodeURIComponent(override)}`;
}

// Desktop mode: SPA loads on the Wails AssetServer origin (wails:// or
// http://wails.localhost), but Wails AssetServer rejects WS upgrades (501).
// We dial the local server directly with the session token in the query.
async function deriveWsUrl(runId: string): Promise<string> {
  if (isDesktop()) {
    const desktopUrl = await getDesktopWsBase(`/api/ws/runs/${encodeURIComponent(runId)}`);
    if (desktopUrl) return appendStoreParam(desktopUrl);
  }
  // In a Wails-hosted page the AssetServer host (window.location.host ===
  // "wails" / "wails.localhost") cannot accept WS upgrades and DNS won't
  // resolve it anyway. If the desktop bindings or the embedded server URL
  // aren't ready yet, surface a transient error so the caller's reconnect
  // logic re-runs deriveWsUrl on the next tick — by then the bindings or
  // the started server should have caught up.
  if (isWailsHosted()) {
    throw new Error("desktop bindings not ready");
  }
  if (BASE_URL.startsWith("http")) {
    return appendStoreParam(BASE_URL.replace(/^http/, "ws") + `/ws/runs/${encodeURIComponent(runId)}`);
  }
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return appendStoreParam(`${proto}//${window.location.host}${BASE_URL}/ws/runs/${encodeURIComponent(runId)}`);
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
  // Ref-count log subscribers so the bottom RunLogPanel and the
  // NodeDetailPanel "Logs" tab can independently subscribe without one
  // unmount canceling the other. We only send subscribe_logs /
  // unsubscribe_logs on the 0↔1 transitions.
  const logSubscriberCountRef = useRef(0);
  // Bump from the store after Resume/Cancel HTTP actions to redial the
  // WS — the broker drops subscribers on terminal status, so the only
  // way the resumed run reaches this client is a fresh subscribe.
  const reconnectToken = useRunStore((s) => s.wsReconnectToken);

  // Track the runId the previous effect run was bound to. The effect
  // re-runs on either runId or reconnectToken change; we use this ref
  // to distinguish them. On a runId switch the consumer panels for
  // the old run will unmount → safe to reset subscription refs. On a
  // reconnectToken bump (post-Resume/Cancel) the same panels stay
  // mounted, so their ref-counted intent must survive — otherwise
  // the new WS opens with count=0 and re-subscribe in onopen is
  // silently skipped, leaving the live log stream dead until the user
  // navigates away and back.
  const prevRunIdRef = useRef<string | null>(null);

  useEffect(() => {
    if (!runId) return;
    aliveRef.current = true;
    reconnectDelay.current = 1000;
    if (prevRunIdRef.current !== runId) {
      // Run changed — wipe inherited subscriber state.
      logSubscriberCountRef.current = 0;
      logsRequestedRef.current = false;
    }
    prevRunIdRef.current = runId;

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

    const connect = async () => {
      if (!aliveRef.current) return;
      setWsState("connecting");
      let url: string;
      try {
        url = await deriveWsUrl(runId);
      } catch {
        // Could not resolve URL (e.g. desktop bindings not yet ready) — fall
        // through to the reconnect timer rather than crashing the run view.
        if (!aliveRef.current) return;
        setWsState("reconnecting");
        scheduleReconnect();
        return;
      }
      if (!aliveRef.current) return; // tear-down raced the await
      const ws = new WebSocket(url);
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
        //
        // replay_history is true ONLY when we already have events
        // locally (i.e., this is a reconnect after an outage): the
        // server fills the gap between FromSeq and snapshotSeq. On
        // initial connect (empty store) we run lazy — snapshot only,
        // then live tail — and let loadEventHistoryIfMissing pull
        // the historical events via HTTP if and when something needs
        // them. Eliminates the 30s replay-stall on first open.
        const events = useRunStore.getState().events;
        const fromSeq =
          events.length > 0 ? events[events.length - 1]!.seq + 1 : 0;
        ws.send(
          JSON.stringify({
            type: "subscribe",
            payload: {
              from_seq: fromSeq,
              replay_history: events.length > 0,
            },
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
            case "event_batch":
              // Server-side bulk envelope (replay path): payload is
              // already an array. Drain the live-event microtask
              // buffer first so seq order is preserved across
              // batches, then push the whole array in one shot —
              // bypasses the per-event microtask round-trip.
              flushEvents();
              applyEventsBatch(env.payload as RunEvent[]);
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
        } catch (err) {
          // A single malformed envelope shouldn't kill the stream, but
          // silently swallowing it hid genuine bugs (reducer crashes on
          // unexpected payload shape) until users reported "the run
          // view is frozen". Log once per error so issues surface in
          // devtools without spamming on a flapping payload.
          // eslint-disable-next-line no-console
          console.warn("[run ws] dropped message:", err);
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
        void connect();
      }, reconnectDelay.current);
    };

    void connect();

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
      // Don't reset subscriber refs here: the next effect body owns
      // that decision via prevRunIdRef. The cleanup runs for both
      // unmount (refs become unreachable → GC) and dependency change
      // (next effect re-evaluates). Resetting unconditionally was the
      // bug — a reconnectToken bump cleared the refs while the same
      // RunLogPanel + NodeDetailPanel Logs consumers were still
      // mounted, then the new ws.onopen saw logsRequestedRef=false
      // and never re-subscribed.
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
      logSubscriberCountRef.current += 1;
      if (logSubscriberCountRef.current > 1) return;
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
      if (logSubscriberCountRef.current === 0) return;
      logSubscriberCountRef.current -= 1;
      if (logSubscriberCountRef.current > 0) return;
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
