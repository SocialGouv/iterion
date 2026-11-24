import { useSyncExternalStore } from "react";

import { runBirthsVersion, subscribeRunBirths } from "@/lib/lostRuns";

// useRunBirthsVersion re-renders its component when the page learns a run's
// birth. A memoized run URL the browser loads by itself (an <img>, <iframe>
// or <track> src) lists it as a dependency, so it is rebuilt to carry the
// expected birth (withExpectedBirth reads the registry at call time).
export function useRunBirthsVersion(): number {
  return useSyncExternalStore(subscribeRunBirths, runBirthsVersion, runBirthsVersion);
}
