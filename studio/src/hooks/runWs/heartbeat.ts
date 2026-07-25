// Application-level heartbeat manager for the run WebSocket (see
// policy.ts for why WS ping frames are not enough). Ticks every
// HEARTBEAT_MS: probes liveness, stands itself down on terminal runs,
// escalates to `onStale` when no inbound frame arrived within
// HEARTBEAT_STALE_MS, and otherwise pings. The hook owns the socket
// and the escalation (forceReconnect); this factory owns the timer.

import { HEARTBEAT_MS, isHeartbeatStale } from "./policy";

export interface HeartbeatController {
  // (Re)arm the interval. Safe to call on an armed controller — the
  // previous timer is cleared first.
  start: () => void;
  stop: () => void;
}

export function createHeartbeat(opts: {
  getSocket: () => WebSocket | null;
  isAlive: () => boolean;
  isTerminal: () => boolean;
  getLastInboundAt: () => number;
  // No inbound frame for too long → the socket is dead even though the
  // browser never surfaced a close. The hook redials.
  onStale: () => void;
  sendPing: (ws: WebSocket) => void;
}): HeartbeatController {
  let timer: ReturnType<typeof setInterval> | null = null;

  const stop = () => {
    if (timer) {
      clearInterval(timer);
      timer = null;
    }
  };

  const start = () => {
    stop();
    timer = setInterval(() => {
      if (!opts.isAlive()) return;
      // Nothing to probe on a finished run — stand the heartbeat down.
      if (opts.isTerminal()) {
        stop();
        return;
      }
      const ws = opts.getSocket();
      if (!ws || ws.readyState !== WebSocket.OPEN) return; // onclose owns this
      if (isHeartbeatStale(Date.now(), opts.getLastInboundAt())) {
        opts.onStale();
        return;
      }
      try {
        opts.sendPing(ws);
      } catch {
        // send raced a close — let the next tick's readyState guard or
        // the watchdog handle it.
      }
    }, HEARTBEAT_MS);
  };

  return { start, stop };
}
