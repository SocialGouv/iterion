// The chat dock's presentation vocabulary, lifted out of the run
// console so it isn't owned by `/runs/:id` any more.
//
// Three states, unchanged from the original FloatingChatPanel:
//   closed        — a bubble in the bottom-right corner
//   floating      — a non-modal resizable panel over the page
//   docked-right  — a column the host lays out beside its content
//
// Persistence is per USER (one localStorage key for the whole studio),
// not per route: the operator who docked the assistant on /board must
// find it docked on /runs. The run console's own steering panel keeps
// its historical per-console key so the two docks stay independent.

import { readEnumFlag, writeStringFlag } from "@/lib/localStorageFlag";

export type DockState = "closed" | "floating" | "docked-right";

export const DOCK_STATES = [
  "closed",
  "floating",
  "docked-right",
] as const satisfies readonly DockState[];

// Tailwind's lg breakpoint — below it the floating panel covers most of
// the viewport, so opening from closed docks instead of floating.
export const DOCK_BREAKPOINT_PX = 1024;

// The assistant dock's persisted state. Distinct from the run console's
// CHAT_DOCK_KEY (`iterion.runview.chatDock`), which belongs to the
// steering panel.
export const ASSISTANT_DOCK_KEY = "iterion.chatDock.assistant";

// openedDock picks the presentation when a dock OPENS from closed.
// Point-in-time check by design: an already-open dock must not re-dock
// itself on window resize, and the user's explicit dock choice (which
// the caller persists) wins afterwards.
//
// `viewportWidth` is injectable so the rule is testable without a DOM;
// callers in the app omit it and get the live window width.
export function openedDock(viewportWidth?: number): DockState {
  const width =
    viewportWidth ??
    (typeof window === "undefined" ? DOCK_BREAKPOINT_PX + 1 : window.innerWidth);
  return width <= DOCK_BREAKPOINT_PX ? "docked-right" : "floating";
}

// readDockState reads a persisted dock state, falling back when the key
// is missing or holds a value from an older build.
export function readDockState(key: string, fallback: DockState = "closed"): DockState {
  return readEnumFlag<DockState>(key, DOCK_STATES, fallback);
}

export function writeDockState(key: string, next: DockState): void {
  writeStringFlag(key, next);
}
