// The launches this session has sent and not yet heard back from, counted by
// what each one runs — the key of the target, as the server resolved it
// (lib/sendLaunch). Kept outside every surface that launches: an answer can be
// awaited longer than its form or its pane is on screen, and on return the
// same target must still be held, or a second click starts a second run.

import { create } from "zustand";

interface LaunchesInFlight {
  byTarget: Record<string, number>;
  /** The account's sitting. It changes when the account does (forget): a
   *  launch sent under the one before is neither held nor answered in this
   *  one. */
  sitting: number;
  begin: (target: string) => void;
  /** Ends a launch begun in `sitting`; one begun in an earlier sitting was
   *  already forgotten, and ends nothing here. */
  end: (target: string, sitting: number) => void;
  forget: () => void;
}

export const useLaunchesInFlight = create<LaunchesInFlight>((set) => ({
  byTarget: {},
  sitting: 0,
  begin: (target) =>
    set((s) => ({ byTarget: { ...s.byTarget, [target]: (s.byTarget[target] ?? 0) + 1 } })),
  end: (target, sitting) =>
    set((s) => {
      if (s.sitting !== sitting) return s;
      const left = (s.byTarget[target] ?? 0) - 1;
      const byTarget = { ...s.byTarget };
      if (left > 0) byTarget[target] = left;
      else delete byTarget[target];
      return { byTarget };
    }),
  forget: () => set((s) => ({ byTarget: {}, sitting: s.sitting + 1 })),
}));
