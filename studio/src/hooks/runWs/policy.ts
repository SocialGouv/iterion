// Connection policy for the run WebSocket: heartbeat/staleness
// thresholds, reconnect backoff, and terminal-status classification.
// Pure constants + predicates; the hook supplies the clocks and refs.

// Application-level heartbeat. The browser auto-answers server WS ping
// FRAMES but never surfaces them to JS, so an idle-but-alive run yields
// zero observable inbound traffic and a half-open socket (peer vanished
// without a FIN — laptop sleep, wifi switch, proxy/NAT idle-drop) is
// never noticed: onclose never fires, no reconnect is scheduled, the
// status pill stays "running" forever. We therefore ping at the JSON-
// envelope layer and watch for ANY inbound frame (event/ack/pong) to
// prove liveness; if none arrives within HEARTBEAT_STALE_MS we force a
// reconnect. HEARTBEAT_STALE_MS tolerates one missed ping plus margin.
export const HEARTBEAT_MS = 20_000;
export const HEARTBEAT_STALE_MS = 45_000;

// Reconnect backoff: 1s doubling to a 30s ceiling.
export const INITIAL_RECONNECT_DELAY_MS = 1000;
export const MAX_RECONNECT_DELAY_MS = 30_000;

export function nextReconnectDelay(currentMs: number): number {
  return Math.min(currentMs * 2, MAX_RECONNECT_DELAY_MS);
}

// Terminal statuses never emit further events, so we stop the heartbeat
// once the run reaches one (pinging a finished run is pointless churn).
const TERMINAL_RUN_STATUSES = new Set(["finished", "failed", "cancelled"]);

export function isTerminalRunStatus(
  status: string | null | undefined,
): boolean {
  return status != null && TERMINAL_RUN_STATUSES.has(status);
}

// Watchdog threshold: no inbound frame for longer than the stale window
// → the socket is dead even though the browser never surfaced a close.
export function isHeartbeatStale(now: number, lastInboundAt: number): boolean {
  return now - lastInboundAt > HEARTBEAT_STALE_MS;
}

// Revalidation threshold (visibility/online wake): more aggressive than
// the watchdog — one missed heartbeat interval already counts as
// suspect, so recovery on tab focus feels instant.
export function isInboundQuiet(now: number, lastInboundAt: number): boolean {
  return now - lastInboundAt > HEARTBEAT_MS;
}
