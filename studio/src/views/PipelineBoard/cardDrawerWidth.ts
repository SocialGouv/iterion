import { readNumberFlag, writeNumberFlag } from "@/lib/localStorageFlag";

// Width model for the pipeline card details drawer. Kept out of the component
// so the clamping rules — the part that actually goes wrong — are unit-tested
// without a DOM.

export const DRAWER_WIDTH_KEY = "pipeline-board.card-drawer-width";
/** 28rem — the historical fixed width, so nothing moves until you drag it. */
export const DRAWER_WIDTH_DEFAULT = 448;
export const DRAWER_WIDTH_MIN = 320;
export const DRAWER_WIDTH_MAX = 1400;
/** Arrow-key step; Shift multiplies it. */
export const DRAWER_WIDTH_STEP = 24;
export const DRAWER_WIDTH_STEP_LARGE = 120;

// drawerWidthBounds narrows the static band by the viewport: a 1400px drawer
// on a 900px screen would push its own resize handle off-screen, and on a
// phone the 320px floor can itself exceed the viewport — so the floor gives
// way to the ceiling rather than producing an empty band.
export function drawerWidthBounds(viewport: number): { min: number; max: number } {
  const vp = Number.isFinite(viewport) && viewport > 0 ? viewport : DRAWER_WIDTH_MAX;
  const max = Math.min(DRAWER_WIDTH_MAX, Math.round(vp));
  return { min: Math.min(DRAWER_WIDTH_MIN, max), max };
}

export function clampDrawerWidth(width: number, viewport: number): number {
  const { min, max } = drawerWidthBounds(viewport);
  if (!Number.isFinite(width)) return Math.min(max, Math.max(min, DRAWER_WIDTH_DEFAULT));
  return Math.min(max, Math.max(min, Math.round(width)));
}

/** Current viewport width, or the static ceiling when there is no window (SSR). */
export function viewportWidth(): number {
  return typeof window === "undefined" ? DRAWER_WIDTH_MAX : window.innerWidth;
}

// readDrawerWidth restores the persisted width, clamped to the CURRENT
// viewport — a width saved on a wide monitor must not strand the drawer
// off-screen on a laptop.
export function readDrawerWidth(viewport: number = viewportWidth()): number {
  return clampDrawerWidth(readNumberFlag(DRAWER_WIDTH_KEY, DRAWER_WIDTH_DEFAULT), viewport);
}

export function writeDrawerWidth(width: number): void {
  writeNumberFlag(DRAWER_WIDTH_KEY, Math.round(width));
}
