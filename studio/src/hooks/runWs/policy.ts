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

// The server names why it closed a socket with a token, the first word of
// the close reason ("<token>: <cause>", pkg/server/hub.go). A client matches
// the token, never the cause behind it.
export function closeReasonToken(reason: string | null | undefined): string {
  if (!reason) return "";
  const end = reason.search(/[:\s]/);
  return end === -1 ? reason : reason.slice(0, end);
}

// scope_lost: what this socket was opened on no longer holds — the run was
// replaced or deleted under its ID, or the caller's standing in its team is
// gone. A redial would attach to whatever run holds the ID now and resume
// from this run's cursors, so the client stops reconnecting instead.
export const SCOPE_LOST_CLOSE_TOKEN = "scope_lost";

export function isScopeLostClose(reason: string | null | undefined): boolean {
  return closeReasonToken(reason) === SCOPE_LOST_CLOSE_TOKEN;
}

// invalid_expected_birth: the dial named an expected birth that is not one.
// The server accepts the socket and closes it with this token instead of
// refusing the handshake, which a browser sees as 1006 and redials; the
// client does not redial either.
export const INVALID_EXPECTATION_CLOSE_TOKEN = "invalid_expected_birth";

export function isInvalidExpectationClose(reason: string | null | undefined): boolean {
  return closeReasonToken(reason) === INVALID_EXPECTATION_CLOSE_TOKEN;
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
