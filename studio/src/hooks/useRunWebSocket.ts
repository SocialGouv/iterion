// Run WebSocket client — the composition shell. The separable concerns
// live under ./runWs/ (url derivation, connection policy, wire
// protocol builders, microtask event coalescing, inbound message
// routing, heartbeat management, alert side-channel); this hook owns
// the ordering-sensitive connection lifecycle: connect/reconnect
// choreography, socket handler wiring, and effect cleanup.

import { useCallback, useEffect, useMemo, useRef } from "react";

import { useRunStore, useRunStoreInstance } from "@/store/run";

import { handleAlertEvent } from "./runWs/alerts";
import { createEventBuffer } from "./runWs/eventBuffer";
import { createHeartbeat } from "./runWs/heartbeat";
import {
  routeRunWsMessage,
  type RunWsMessageSinks,
} from "./runWs/messageRouter";
import {
  INITIAL_RECONNECT_DELAY_MS,
  isInboundQuiet,
  isTerminalRunStatus,
  nextReconnectDelay,
} from "./runWs/policy";
import {
  pingEnvelope,
  resubscribeLogsEnvelope,
  subscribeEnvelope,
  subscribeLogsEnvelope,
  unsubscribeEnvelope,
  unsubscribeLogsEnvelope,
  type WsEnvelope,
} from "./runWs/protocol";
import { deriveWsUrl } from "./runWs/url";

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
  const reconnectDelay = useRef(INITIAL_RECONNECT_DELAY_MS);
  const aliveRef = useRef(false);
  // Timestamp (ms) of the last inbound frame — updated on every
  // onmessage. The heartbeat watchdog compares against it to detect a
  // silently-dead socket. Bumped on connect so a fresh socket starts
  // with a clean clock.
  const lastInboundAtRef = useRef(0);
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

  // Capture the active RunStore instance (the per-run store provided
  // by RunTabHost, or the module default when no Provider is mounted).
  // We freeze it into a ref so reconnects fire against the same store
  // even if the surrounding Context changes mid-flight.
  const store = useRunStoreInstance();
  const runStoreRef = useRef(store);
  runStoreRef.current = store;

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
    reconnectDelay.current = INITIAL_RECONNECT_DELAY_MS;
    if (prevRunIdRef.current !== runId) {
      // Run changed — wipe inherited subscriber state.
      logSubscriberCountRef.current = 0;
      logsRequestedRef.current = false;
    }
    prevRunIdRef.current = runId;

    const store = runStoreRef.current;
    const setWsState = store.getState().setWsState;
    const applySnapshot = store.getState().applySnapshot;
    const applyEventsBatch = store.getState().applyEventsBatch;
    const applyLogChunk = store.getState().applyLogChunk;
    const markLogTerminated = store.getState().markLogTerminated;
    const setLogSubscribed = store.getState().setLogSubscribed;

    // Coalesce events that arrive in the same microtask before pushing
    // them to the store (see createEventBuffer for the O(N²) rationale).
    const eventBuffer = createEventBuffer(applyEventsBatch);

    const runIsTerminal = () =>
      isTerminalRunStatus(runStoreRef.current.getState().snapshot?.run.status);

    // Heartbeat watchdog: pings on an interval and escalates a stale
    // socket to forceReconnect (see ./runWs/heartbeat.ts + policy.ts).
    const heartbeat = createHeartbeat({
      getSocket: () => wsRef.current,
      isAlive: () => aliveRef.current,
      isTerminal: runIsTerminal,
      getLastInboundAt: () => lastInboundAtRef.current,
      onStale: () => forceReconnect(),
      sendPing: (ws) => ws.send(JSON.stringify(pingEnvelope())),
    });

    // Inbound routing targets. Alert events go out-of-band (never into
    // the seq-ordered store — see ./runWs/alerts.ts); `terminated`
    // keeps the socket open (the server closes it eventually) but
    // stops the heartbeat — a finished run needs no liveness probing.
    const sinks: RunWsMessageSinks = {
      applySnapshot,
      queueEvent: eventBuffer.queue,
      flushEvents: eventBuffer.flush,
      applyEventsBatch,
      applyLogChunk,
      markLogTerminated,
      onAlertEvent: handleAlertEvent,
      onTerminated: () => {
        heartbeat.stop();
        setWsState("closed");
      },
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
        reconnectDelay.current = INITIAL_RECONNECT_DELAY_MS;
        // Fresh socket → reset the liveness clock and arm the heartbeat.
        lastInboundAtRef.current = Date.now();
        heartbeat.start();

        // Resume from the highest seq the store has actually consumed;
        // replay history only on reconnect (see subscribeEnvelope).
        const events = runStoreRef.current.getState().events;
        ws.send(JSON.stringify(subscribeEnvelope(events)));

        // Re-subscribe to logs if the user had opened the Logs tab
        // before the disconnect. We resume from the byte after our
        // last known position so the backend snapshot fills any gap
        // that landed during the outage.
        if (logsRequestedRef.current) {
          const log = runStoreRef.current.getState().log;
          // Byte-accurate resume cursor — NOT start + text.length (UTF-16
          // code units), which drifts below the true byte offset on the
          // run console's multi-byte glyphs and made the backend resend
          // overlapping tails that the client re-appended as duplicates.
          const fromOffset = log.nextByte;
          ws.send(JSON.stringify(resubscribeLogsEnvelope(fromOffset)));
          setLogSubscribed(true);
        }
      };

      ws.onmessage = (msgEv) => {
        // ANY inbound frame proves the connection is alive — record it
        // before parsing so a malformed payload still counts as liveness.
        lastInboundAtRef.current = Date.now();
        try {
          const env = JSON.parse(msgEv.data) as WsEnvelope;
          routeRunWsMessage(env, sinks);
        } catch (err) {
          // A single malformed envelope shouldn't kill the stream, but
          // silently swallowing it hid genuine bugs (reducer crashes on
          // unexpected payload shape) until users reported "the run
          // view is frozen". Log once per error so issues surface in
          // devtools without spamming on a flapping payload.
          console.warn("[run ws] dropped message:", err);
        }
      };

      ws.onclose = () => {
        // Drop the wsRef only if it still points at THIS socket — a
        // rapid runId change can race two connect() invocations, and
        // we don't want the older socket's late onclose to null out
        // the newer one.
        if (wsRef.current === ws) wsRef.current = null;
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
      // Defensive: clear any timer still armed from a previous failure path
      // before scheduling a new one. Two stacked onclose handlers (e.g. when
      // a reconnectToken bump races an in-flight close on the prior socket)
      // can otherwise double-arm and accumulate backoff. Mirrors the same
      // guard in api/ws.ts.
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
      reconnectTimer.current = setTimeout(() => {
        reconnectTimer.current = null;
        reconnectDelay.current = nextReconnectDelay(reconnectDelay.current);
        void connect();
      }, reconnectDelay.current);
    };

    // forceReconnect tears down the current socket and dials a fresh one
    // WITHOUT waiting for onclose. On a half-open socket ws.close() may
    // not fire onclose for minutes (or ever), so the watchdog and the
    // visibility/online listeners can't route recovery through the normal
    // onclose→scheduleReconnect path — they must redial directly.
    const forceReconnect = () => {
      if (!aliveRef.current || runIsTerminal()) return;
      heartbeat.stop();
      if (reconnectTimer.current) {
        clearTimeout(reconnectTimer.current);
        reconnectTimer.current = null;
      }
      const stale = wsRef.current;
      if (stale) {
        // Detach handlers so the doomed socket's late onclose can't
        // schedule a competing reconnect against the new one.
        stale.onopen = null;
        stale.onmessage = null;
        stale.onerror = null;
        stale.onclose = null;
        try {
          stale.close();
        } catch {
          // already closing/closed
        }
        wsRef.current = null;
      }
      reconnectDelay.current = INITIAL_RECONNECT_DELAY_MS;
      setWsState("reconnecting");
      void connect();
    };

    // Proactive recovery on the two events that most often coincide with
    // a silently-dropped socket: the tab regaining focus (laptop wake,
    // app switch) and the network coming back. Both fire long before the
    // ~45s watchdog would, so recovery feels instant. We only redial when
    // the connection actually looks stale/down and the run is still live.
    const revalidate = () => {
      if (!aliveRef.current || runIsTerminal()) return;
      const wsDown = runStoreRef.current.getState().wsState !== "open";
      const stale = isInboundQuiet(Date.now(), lastInboundAtRef.current);
      if (wsDown || stale) forceReconnect();
    };
    const onVisibility = () => {
      if (document.visibilityState === "visible") revalidate();
    };
    const onOnline = () => revalidate();
    document.addEventListener("visibilitychange", onVisibility);
    window.addEventListener("online", onOnline);

    void connect();

    return () => {
      aliveRef.current = false;
      document.removeEventListener("visibilitychange", onVisibility);
      window.removeEventListener("online", onOnline);
      heartbeat.stop();
      // Drain any events buffered for the next microtask so we don't
      // lose them when React unmounts the hook before the flush fires.
      eventBuffer.flush();
      if (reconnectTimer.current) {
        clearTimeout(reconnectTimer.current);
        reconnectTimer.current = null;
      }
      const ws = wsRef.current;
      if (ws) {
        // Detach our handlers BEFORE closing so the in-flight FIN
        // can't fire a stale onclose that would observe aliveRef=true
        // (set by a re-mount on rapid navigation) and schedule a
        // bogus reconnect on a dangling socket.
        ws.onopen = null;
        ws.onmessage = null;
        ws.onerror = null;
        ws.onclose = null;
        try {
          ws.send(JSON.stringify(unsubscribeEnvelope()));
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
      runStoreRef.current.getState().setWsState("closed");
    };
  }, [runId, reconnectToken]);

  // Stable handle: every callback closes over refs only (no props/state), so
  // useCallback([]) keeps their identity constant across renders, and useMemo
  // keeps the returned object identity stable. Consumers (e.g. LogLinesView)
  // subscribe/unsubscribe in a mount-only effect keyed on these — an unstable
  // handle made that effect tear down and re-run on every render, churning
  // subscribe_logs/unsubscribe_logs and re-anchoring the backend log tail.
  const send = useCallback((env: WsEnvelope) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(env));
    }
  }, []);
  const subscribeLogs = useCallback((fromOffset?: number) => {
    logSubscriberCountRef.current += 1;
    if (logSubscriberCountRef.current > 1) return;
    logsRequestedRef.current = true;
    const ws = wsRef.current;
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      // onopen re-fires subscribe_logs when logsRequestedRef is set,
      // so the only path missed by the early return is the unusual
      // case where ws closed between subscribeLogs() call and the
      // socket actually being open. Logged so a future regression is
      // visible in DevTools. F-NEW-3 instrumentation.
      console.warn("[useRunWebSocket] subscribe_logs deferred: ws not open", {
        readyState: ws?.readyState ?? "no_ws",
      });
      return;
    }
    const offset =
      typeof fromOffset === "number"
        ? fromOffset
        : (() => {
            const log = runStoreRef.current.getState().log;
            // Byte-accurate cursor (see the onopen reconnect path).
            return log.nextByte;
          })();
    ws.send(JSON.stringify(subscribeLogsEnvelope(offset)));
    runStoreRef.current.getState().setLogSubscribed(true);
  }, []);
  const unsubscribeLogs = useCallback(() => {
    if (logSubscriberCountRef.current === 0) return;
    logSubscriberCountRef.current -= 1;
    if (logSubscriberCountRef.current > 0) return;
    logsRequestedRef.current = false;
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(unsubscribeLogsEnvelope()));
    }
    runStoreRef.current.getState().setLogSubscribed(false);
  }, []);

  return useMemo(
    () => ({ send, subscribeLogs, unsubscribeLogs }),
    [send, subscribeLogs, unsubscribeLogs],
  );
}
