// The bubble's "new messages" badge, for a dock that is currently closed.
//
// Lives here rather than beside its one caller because getting it right
// needs a test, and the interesting case — a session that hydrates AFTER
// the dock has already mounted — is a hook-lifecycle case, not a
// rendering one.

import { useLayoutEffect, useState } from "react";

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
  const [committed, setCommitted] = useState(() => ({
    baseline: messageCount,
    hydrated: messageCount > 0,
  }));

  // Derive the next tracker during render so the badge is right in THIS
  // frame, then commit it in a layout effect. Mutating refs during render
  // looked equivalent but is not under concurrent rendering: a render that
  // suspends and is discarded still mutates its refs, so an open-dock render
  // that never committed could silently mark messages as read.
  let next = committed;
  if (!committed.hydrated) {
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
    if (
      messageCount !== committed.baseline ||
      (messageCount > 0) !== committed.hydrated
    ) {
      next = { baseline: messageCount, hydrated: messageCount > 0 };
    }
  } else if (messageCount < committed.baseline) {
    // The transcript SHRANK. A conversation only grows, so this is not
    // arrival — it is the session being replaced: useWhatsNextSession
    // drops the run and resets the store when the project or repo scope
    // changes. Keeping the old baseline would measure the new
    // conversation against the length of a transcript that no longer
    // exists, holding the badge at 0 until the new one grew past it.
    //
    // Re-baselining onto the new (usually empty) transcript also clears
    // `hydrated`, which puts the fresh session back through the startup
    // branch above — so its own 0 → N restore is treated as a restore
    // rather than as N new messages, same as on a cold mount.
    next = { baseline: messageCount, hydrated: messageCount > 0 };
  } else if (!closed && messageCount !== committed.baseline) {
    // While open, everything on screen is read, so the baseline tracks
    // the count. It therefore holds the count as of the moment the dock
    // closed — exactly what the badge must measure against.
    next = { baseline: messageCount, hydrated: true };
  }

  useLayoutEffect(() => {
    if (
      next.baseline !== committed.baseline ||
      next.hydrated !== committed.hydrated
    ) {
      // This is an intentional committed-props tracker. The value was already
      // used to render the correct current frame; the layout update only makes
      // that committed baseline authoritative for the next render.
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setCommitted(next);
    }
  }, [committed, next]);

  return closed ? Math.max(0, messageCount - next.baseline) : 0;
}
