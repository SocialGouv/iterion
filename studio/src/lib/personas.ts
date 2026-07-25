/**
 * Visual identity for the iterion bot team — an emoji avatar + accent
 * colour each bot is rendered with across the studio (run-header chip,
 * board bot picker, chat bubbles).
 *
 * PRESENTATION ONLY. The bot's *name* is the manifest `display_name`
 * (carried on the run as `bundle_display_name` and on the registry Entry
 * as `display_name`); this map never holds the authoritative name, only
 * the emoji + colour that go with it. Keys are the bot's CANONICAL
 * technical id (kebab); lookups normalise snake_case → kebab so
 * `feature_dev` and `feature-dev` both resolve.
 */

export interface BotIdentity {
  /** Single emoji standing in for the bot's avatar. */
  emoji: string;
  /** Persona token text-colour class (`text-persona-*`). A categorical
   *  identity hue tokenised in app.css (dark = Tailwind-400, light = -700)
   *  so it keeps AA contrast on light surfaces. */
  color: string;
}

const BOT_IDENTITY: Record<string, BotIdentity> = {
  "whats-next": { emoji: "🧭", color: "text-persona-sky" },
  "feature-dev": { emoji: "🛠️", color: "text-persona-emerald" },
  "branch-improve-loop": { emoji: "🌿", color: "text-persona-violet" },
  "whole-improve-loop": { emoji: "🌍", color: "text-persona-teal" },
  "docs-refresh": { emoji: "📚", color: "text-persona-amber" },
  "doc-align": { emoji: "📚", color: "text-persona-amber" }, // back-compat alias (renamed 2026-06); keeps Doki's identity on pre-rename runs
  "review-pr": { emoji: "🔎", color: "text-persona-cyan" },
  "sec-audit-source": { emoji: "🛡️", color: "text-persona-rose" },
  "sec-audit-deps": { emoji: "📦", color: "text-persona-orange" },
  "secured-renovacy": { emoji: "⬆️", color: "text-persona-lime" },
  "adr-cartograph": { emoji: "🗺️", color: "text-persona-teal" },
  "adr-rechallenge": { emoji: "🏛️", color: "text-persona-violet" },
  bmady: { emoji: "📋", color: "text-persona-amber" },
  "dep-update-guard": { emoji: "🦺", color: "text-persona-orange" },
  "devbox-setup": { emoji: "⚙️", color: "text-persona-sky" },
  evolve: { emoji: "🔮", color: "text-persona-violet" },
  "feature-gap-fill": { emoji: "🧩", color: "text-persona-emerald" },
  "revi-converse": { emoji: "💬", color: "text-persona-cyan" },
  "rgaa-audit": { emoji: "♿", color: "text-persona-sky" },
  "supply-shield": { emoji: "🔰", color: "text-persona-lime" },
  "supply-shield-cve": { emoji: "🚨", color: "text-persona-rose" },
  "test-coverage": { emoji: "🧪", color: "text-persona-emerald" },
};

const FALLBACK_COLORS = [
  "text-persona-sky",
  "text-persona-emerald",
  "text-persona-violet",
  "text-persona-amber",
  "text-persona-rose",
  "text-persona-cyan",
  "text-persona-orange",
  "text-persona-lime",
];

/** Canonicalise a technical bot id: lower-case, collapse runs of `_`/`-`
 *  into a single `-`. So `feature_dev`, `feature-dev` and `Feature_Dev`
 *  all map to the same key. */
export function canon(name: string): string {
  return name.trim().toLowerCase().replace(/[_-]+/g, "-");
}

/**
 * botIdentity resolves a bot's emoji + accent colour from its technical id
 * (kebab or snake). Unknown bots get a deterministic fallback — a stable
 * colour derived from the name hash plus a generic robot emoji — so every
 * bot gets a distinct chip and the UI never renders a bare one. The NAME
 * is not this map's concern: read the manifest display_name (a run's
 * bundle_display_name, or the registry entry's display_name).
 */
export function botIdentity(name: string | undefined | null): BotIdentity {
  const key = canon(name ?? "");
  const hit = BOT_IDENTITY[key];
  if (hit) return hit;
  let h = 0;
  for (let i = 0; i < key.length; i++) h = (h * 31 + key.charCodeAt(i)) >>> 0;
  return {
    emoji: "🤖",
    color: FALLBACK_COLORS[h % FALLBACK_COLORS.length] ?? "text-persona-sky",
  };
}

/**
 * botVisual resolves a bot's visual identity from a registry entry.
 * Emoji precedence: the manifest `icon` (entry.icon, operator-authored)
 * wins over the built-in BOT_IDENTITY map, which wins over the generic
 * hash fallback. The accent COLOUR always comes from the persona/hash
 * path (`botIdentity`) — a manifest icon carries no colour, so the chip
 * hue stays stable whether or not an icon is set.
 */
export function botVisual(entry: { name: string; icon?: string }): BotIdentity {
  const base = botIdentity(entry.name);
  const icon = entry.icon?.trim();
  if (icon) return { emoji: icon, color: base.color };
  return base;
}
