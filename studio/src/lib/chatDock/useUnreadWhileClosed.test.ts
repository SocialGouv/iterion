// @vitest-environment jsdom
import { renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { DockState } from "./dockState";
import { useUnreadWhileClosed } from "./useUnreadWhileClosed";

describe("useUnreadWhileClosed", () => {
  it("counts messages that arrive while the dock is closed", () => {
    const { result, rerender } = renderHook(
      ({ count }) => useUnreadWhileClosed("closed", count),
      { initialProps: { count: 3 } },
    );
    expect(result.current).toBe(0);

    rerender({ count: 5 });
    expect(result.current).toBe(2);
  });

  it("reports nothing while the dock is open — on screen is read", () => {
    const { result, rerender } = renderHook(
      ({ count }) => useUnreadWhileClosed("floating", count),
      { initialProps: { count: 3 } },
    );
    rerender({ count: 9 });
    expect(result.current).toBe(0);
  });

  it("measures from the count at the moment the dock closed", () => {
    const { result, rerender } = renderHook(
      ({ dock, count }: { dock: DockState; count: number }) =>
        useUnreadWhileClosed(dock, count),
      { initialProps: { dock: "floating" as DockState, count: 4 } },
    );
    // Read while open, then closed, then two more arrive.
    rerender({ dock: "closed", count: 4 });
    rerender({ dock: "closed", count: 6 });
    expect(result.current).toBe(2);

    // Re-opening clears the badge and re-baselines.
    rerender({ dock: "floating", count: 6 });
    expect(result.current).toBe(0);
    rerender({ dock: "closed", count: 7 });
    expect(result.current).toBe(1);
  });

  // The regression this hook was extracted for. The dock state is
  // persisted per user, so a page load can mount it already closed with
  // the session not yet attached — first render sees 0 messages, then
  // startup discovery hydrates the whole restored transcript at once.
  // Baselining on that placeholder zero made the bubble announce the
  // operator's own already-read conversation as new.
  it("does not count a transcript restored after mount as new", () => {
    const { result, rerender } = renderHook(
      ({ count }) => useUnreadWhileClosed("closed", count),
      { initialProps: { count: 0 } },
    );
    expect(result.current).toBe(0);

    // Discovery attaches and hydrates 12 restored messages.
    rerender({ count: 12 });
    expect(result.current).toBe(0);

    // Only what genuinely arrives afterwards counts.
    rerender({ count: 13 });
    expect(result.current).toBe(1);
  });

  it("takes the restored count in the same render it hydrates", () => {
    // Guards the reason this is derived during render instead of in an
    // effect: an effect would set the baseline one commit late, and
    // since a ref write triggers no re-render the wrong count would
    // stick. Asserted on the FIRST result after the jump, with no
    // intervening rerender to paper over a stale frame.
    const { result, rerender } = renderHook(
      ({ count }) => useUnreadWhileClosed("closed", count),
      { initialProps: { count: 0 } },
    );
    rerender({ count: 40 });
    expect(result.current).toBe(0);
  });

  // Switching project/repo scope makes useWhatsNextSession drop the run
  // and reset the store, so the transcript goes N → 0. Measuring the new
  // conversation against the old one's length would hold the badge at 0
  // until it grew past it.
  it("re-baselines when the session is replaced under it", () => {
    const { result, rerender } = renderHook(
      ({ count }) => useUnreadWhileClosed("closed", count),
      { initialProps: { count: 0 } },
    );
    rerender({ count: 12 });
    expect(result.current).toBe(0);

    // Scope switch: the store resets.
    rerender({ count: 0 });
    expect(result.current).toBe(0);

    // The new scope's own restore is a restore, not arrival — same rule
    // as a cold mount.
    rerender({ count: 3 });
    expect(result.current).toBe(0);

    // And what arrives after it counts from one, not from thirteen.
    rerender({ count: 4 });
    expect(result.current).toBe(1);
  });
});
