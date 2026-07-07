import { useEffect, useState } from "react";

// useNow returns a millisecond timestamp that re-renders on `intervalMs`
// while non-null, and snaps once (then stops ticking) when it flips to
// null. This is the run console's live-duration pattern: tick every
// second while a run is active, freeze the final value the moment it
// ends so a finished run doesn't re-render forever.
export function useNow(intervalMs: number | null): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (intervalMs === null) {
      // Snap once so the final duration is captured, then stop ticking.
      setNow(Date.now());
      return;
    }
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
}
