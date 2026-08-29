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

import { readEnumFlag, readStringFlag, writeStringFlag } from "@/lib/localStorageFlag";

export type DockState = "closed" | "floating" | "docked-right";

export const DOCK_STATES = [
  "closed",
  "floating",
  "docked-right",
] as const satisfies readonly DockState[];

// Below tablet width the assistant behaves as an overlaying side sheet.
// At 1024px a 380px reserved column plus the sidebar left too little room
// for the page, so medium/desktop widths open the resizable floating panel.
export const DOCK_BREAKPOINT_PX = 768;
// The run steering panel docks inside its own resizable SideDock rather than
// reserving a fixed app column. Its older lg threshold is still the right one:
// below it a floating panel covers the run canvas and bottom tab bar.
export const STEERING_DOCK_BREAKPOINT_PX = 1024;

// The assistant dock's persisted state. Distinct from the run console's
// CHAT_DOCK_KEY (`iterion.runview.chatDock`), which belongs to the
// steering panel.
export const ASSISTANT_DOCK_KEY = "iterion.chatDock.assistant";

// Which conversational bot answers in the dock. Persisted per browser so an
// operator who switched to the iterion copilot finds it there next time; an
// unknown or removed id falls back to the registry's default rather than
// leaving the dock empty.
export const ASSISTANT_BOT_KEY = "iterion.chatDock.assistantBot";

// openedDock picks the presentation when a dock OPENS from closed.
// Point-in-time check by design: an already-open dock must not re-dock
// itself on window resize, and the user's explicit dock choice (which
// the caller persists) wins afterwards.
//
// `viewportWidth` is injectable so the rule is testable without a DOM;
// callers in the app omit it and get the live window width.
export function openedDock(
  viewportWidth?: number,
  breakpoint: number = DOCK_BREAKPOINT_PX,
): DockState {
  const width =
    viewportWidth ??
    (typeof window === "undefined" ? breakpoint + 1 : window.innerWidth);
  return width <= breakpoint ? "docked-right" : "floating";
}

// readDockState reads a persisted dock state, falling back when the key
// is missing or holds a value from an older build.
export function readDockState(key: string, fallback: DockState = "closed"): DockState {
  return readEnumFlag<DockState>(key, DOCK_STATES, fallback);
}

export function writeDockState(key: string, next: DockState): void {
  writeStringFlag(key, next);
}

// How wide the docked-right column is, in px. Remembered per browser like the
// dock state itself: a width is a workspace preference, not a per-page one.
//
// The value is LOAD-BEARING beyond the panel: AppShell reserves exactly this
// much padding so the dock pushes the page aside instead of covering it. Any
// surface that reserves or insets layout must read the same number, or the
// page and the dock disagree about where the edge is.
export const ASSISTANT_DOCK_WIDTH_KEY = "iterion.chatDock.assistantWidth";

// The out-of-the-box column width. ChatDockShell re-exports it as
// DOCKED_WIDTH_PX for its existing callers.
export const DOCKED_WIDTH_DEFAULT_PX = 380;

export const DOCK_MIN_WIDTH_PX = 320;
// A ceiling rather than a free drag: past roughly two thirds of the viewport
// the "pushes the page aside" promise stops being true — the page is what is
// left, and there would be almost none of it.
export const DOCK_MAX_WIDTH_FRACTION = 0.7;

export function maxDockWidthPx(viewportWidth?: number): number {
  const w =
    viewportWidth ?? (typeof window === "undefined" ? 1280 : window.innerWidth);
  return Math.max(DOCK_MIN_WIDTH_PX, Math.round(w * DOCK_MAX_WIDTH_FRACTION));
}

export function clampDockWidth(px: number, viewportWidth?: number): number {
  if (!Number.isFinite(px)) return DOCK_MIN_WIDTH_PX;
  return Math.min(
    maxDockWidthPx(viewportWidth),
    Math.max(DOCK_MIN_WIDTH_PX, Math.round(px)),
  );
}

// A stored width from a wider monitor must not strand the dock over the whole
// page on a narrow one, so the clamp is applied on READ as well as on write.
export function readDockWidth(fallback: number, viewportWidth?: number): number {
  const raw = readStringFlag(ASSISTANT_DOCK_WIDTH_KEY, "");
  if (!raw) return clampDockWidth(fallback, viewportWidth);
  const parsed = Number.parseInt(raw, 10);
  if (!Number.isFinite(parsed)) return clampDockWidth(fallback, viewportWidth);
  return clampDockWidth(parsed, viewportWidth);
}

export function writeDockWidth(px: number): void {
  writeStringFlag(ASSISTANT_DOCK_WIDTH_KEY, String(clampDockWidth(px)));
}

// The FLOATING panel's size. The browser's own `resize` handle already let the
// operator drag it; nothing remembered the result, so every reopen threw the
// adjustment away. Keyed per surface (assistant / steering) because the two
// panels are different jobs and deserve different shapes.
export interface DockSize {
  width: number;
  height: number;
}

export const ASSISTANT_FLOATING_SIZE_KEY = "iterion.chatDock.assistantSize";

export const FLOATING_MIN_WIDTH_PX = 320;
export const FLOATING_MIN_HEIGHT_PX = 280;

export function clampDockSize(
  size: DockSize,
  viewport?: { width: number; height: number },
): DockSize {
  const vw = viewport?.width ?? (typeof window === "undefined" ? 1280 : window.innerWidth);
  const vh = viewport?.height ?? (typeof window === "undefined" ? 800 : window.innerHeight);
  return {
    width: Math.min(Math.max(FLOATING_MIN_WIDTH_PX, Math.round(size.width)), vw),
    height: Math.min(Math.max(FLOATING_MIN_HEIGHT_PX, Math.round(size.height)), vh),
  };
}

export function readDockSize(key: string, fallback: DockSize): DockSize {
  const raw = readStringFlag(key, "");
  if (!raw) return clampDockSize(fallback);
  const parts = raw.split("x");
  const w = Number.parseInt(parts[0] ?? "", 10);
  const h = Number.parseInt(parts[1] ?? "", 10);
  if (!Number.isFinite(w) || !Number.isFinite(h)) return clampDockSize(fallback);
  return clampDockSize({ width: w, height: h });
}

export function writeDockSize(key: string, size: DockSize): void {
  const c = clampDockSize(size);
  writeStringFlag(key, `${c.width}x${c.height}`);
}
