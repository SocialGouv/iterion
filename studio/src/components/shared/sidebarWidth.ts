// The sidebar rail's two widths, as Tailwind arbitrary-value literals in
// rem so the rail grows with the operator's font size: at 200 % text
// (RGAA 10.4.1, #2522) the nav labels wrap inside a rail that widened with
// them instead of losing text to a fixed pixel box. Tailwind's scanner
// only sees string literals, so the classes stay literal here…
export const SIDEBAR_W_EXPANDED = "w-[13.75rem]";
export const SIDEBAR_W_COLLAPSED = "w-[3.5rem]";

// …and the pixel figure a FIXED surface needs (the floating assistant's
// resize clamp, which grows leftward and must stop at the rail) is DERIVED
// from the literal through the live root font size — never duplicated as a
// px constant that a larger text setting would silently leave behind.
export function sidebarWidthPx(collapsed: boolean): number {
  const widthClass = collapsed ? SIDEBAR_W_COLLAPSED : SIDEBAR_W_EXPANDED;
  const rem = /^w-\[([\d.]+)rem\]$/.exec(widthClass)?.[1];
  if (rem === undefined) {
    throw new Error(`sidebar width must be a w-[Nrem] literal, got ${widthClass}`);
  }
  const root =
    typeof document === "undefined"
      ? 16
      : Number.parseFloat(getComputedStyle(document.documentElement).fontSize);
  return Number(rem) * (Number.isFinite(root) && root > 0 ? root : 16);
}
