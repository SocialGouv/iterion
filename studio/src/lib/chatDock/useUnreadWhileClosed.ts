// The bubble's "new messages" badge, for a dock that is currently closed.
//
// Lives here rather than beside its one caller because getting it right
// needs a test, and the interesting case — a session that hydrates AFTER
// the dock has already mounted — is a hook-lifecycle case, not a
// rendering one.

import { useRef } from "react";

import type { DockState } from "./dockState";

/**
 * useUnreadWhileClosed counts the messages that arrived since the dock
 * was closed. Returns 0 whenever the dock is open — what is on screen is
 * read by definition.
 */
export function useUnreadWhileClosed(
  dock: DockState,
  messageCount: number,
): number {
  const closed = dock === "closed";
  const baselineRef = useRef(messageCount);
  // Has the session ever shown us a transcript? Until it has, we have no
  // baseline worth measuring against — only a placeholder zero.
  const hydratedRef = useRef(messageCount > 0);

  // Derived during render rather than in an effect, deliberately. An
  // effect would set the baseline one commit AFTER the render that
  // already computed the badge from the stale one, so the count would
  // flash the wrong number and — since a ref write triggers no re-render
  // — stay there until something else re-rendered the dock.
  if (!hydratedRef.current) {
    // Startup. The dock state is persisted per user, so it can mount
    // already `closed` while the session is still attaching: the first
    // render sees 0 messages. Taking that as the baseline would count
    // the whole RESTORED transcript as new the moment discovery
    // hydrates it — the badge announcing "12 new" for a conversation
    // the operator has already read.
    //
    // A transcript going 0 → N is always a restore, never arrival: the
    // assistant cannot answer into an empty transcript without the
    // operator having written to it first, and writing needs an open
    // surface (the dock, or the /whats-next route where the dock stands
    // down and this hook is not mounted at all). So the baseline
    // follows the count until the count is first non-zero.
    baselineRef.current = messageCount;
    hydratedRef.current = messageCount > 0;
  } else if (!closed) {
    // While open, everything on screen is read, so the baseline tracks
    // the count. It therefore holds the count as of the moment the dock
    // closed — exactly what the badge must measure against.
    baselineRef.current = messageCount;
  }

  return closed ? Math.max(0, messageCount - baselineRef.current) : 0;
}
