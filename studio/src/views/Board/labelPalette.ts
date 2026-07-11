import { softColor } from "@/lib/constants";

// labelPalette derives a stable pastel background + foreground colour
// from a label name. Two cards with the label "urgent" always render
// the same colour, but "infra" and "urgent" land on visibly distinct
// palettes. Hashing avoids the need for a label-colour schema in the
// backend — operators get colour scanning today without configuration.
// A small alias table covers common semantic labels with sensible
// presets (red for "urgent" / "bug", green for "ready", etc.).
// Token-driven alias table at module scope: built once, not per-label
// per-card per-render. Severity labels reuse the prebuilt design-system
// *-soft pairs (single source of truth for the 18% tint); docs borrows the
// iteration-1 (purple) hue, which has no -soft token, via softColor. The
// chips invert correctly in light mode because the values are CSS vars.
const DANGER_CHIP = { backgroundColor: "var(--color-danger-soft)", color: "var(--color-danger-fg)" };
const SUCCESS_CHIP = { backgroundColor: "var(--color-success-soft)", color: "var(--color-success-fg)" };
const LABEL_ALIASES: Record<string, { backgroundColor: string; color: string }> = {
  urgent: DANGER_CHIP,
  blocker: DANGER_CHIP,
  bug: DANGER_CHIP,
  infra: { backgroundColor: "var(--color-info-soft)", color: "var(--color-info-fg)" },
  docs: { backgroundColor: softColor("var(--color-iteration-1)", 18), color: "var(--color-fg-default)" },
  feature: SUCCESS_CHIP,
  ready: SUCCESS_CHIP,
};

export function labelPalette(label: string): { backgroundColor: string; color: string } {
  const hit = LABEL_ALIASES[label.toLowerCase()];
  if (hit) return hit;
  // Stable 32-bit FNV-1a hash → hue. Fixed S/L keeps the palette readable
  // against both light and dark surfaces.
  let h = 2166136261 >>> 0;
  for (let i = 0; i < label.length; i++) {
    h ^= label.charCodeAt(i);
    h = Math.imul(h, 16777619) >>> 0;
  }
  const hue = h % 360;
  return {
    backgroundColor: `hsl(${hue}, 60%, 28%)`,
    color: `hsl(${hue}, 80%, 88%)`,
  };
}

// pickPinnedFields returns up to two scalar field entries from a card's
// `fields` map so the card body can surface high-signal data (enum
// statuses, customer IDs) inline without expanding the modal. Skips
// fields whose value is too long for a card row — those belong in the
// hover preview / modal view.
export function pickPinnedFields(fields: Record<string, unknown>): Array<[string, unknown]> {
  const picked: Array<[string, unknown]> = [];
  for (const [k, v] of Object.entries(fields)) {
    if (picked.length >= 2) break;
    if (v === null || v === undefined) continue;
    if (typeof v === "object") continue;
    const str = String(v);
    if (str.length === 0 || str.length > 32) continue;
    picked.push([k, v]);
  }
  return picked;
}

export function shortID(id: string) {
  const bare = id.replace(/^native:/, "").replace(/^github:[^#]+#/, "#");
  return bare.length > 10 ? bare.slice(0, 10) : bare;
}
