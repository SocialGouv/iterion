// Card tags: the operator-visible chip vocabulary for a pipeline card.
//
// Sources (union, de-duplicated, stable order):
//  1. Native issue labels (authored on the ticket / by bots)
//  2. Content-derived tags from entry_input — so run-only cards (no issue)
//     still carry filterable chips explaining WHAT they produce
//
// Used by: card face chips, filter vocabulary, label-filter matching.
// Keep this list short and human — long prose (hook/angle/summary) is never
// a tag; it belongs in the title or the drawer.

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import { cardArg } from "./filters";

/** Max chips rendered on the card face before "+N". */
export const CARD_TAG_FACE_LIMIT = 4;

// cardTags returns the full ordered tag list for a card.
export function cardTags(card: PipelineBoardCard): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const push = (raw: string | undefined | null) => {
    const t = (raw ?? "").trim();
    if (!t) return;
    // Guard against path-like / huge values sneaking in via free-form labels.
    // Episode chips like "ÉP 1/5" are fine; "assets/foo/bar" is not.
    if (t.length > 48) return;
    if (looksLikePathTag(t)) return;
    const key = t.toLowerCase();
    if (seen.has(key)) return;
    seen.add(key);
    out.push(t);
  };

  for (const l of card.labels ?? []) push(l);

  // Subject / series framing (shorts, humanoid, mesh).
  push(cardArg(card, "character"));
  push(cardArg(card, "requested_character"));
  push(cardArg(card, "subject"));
  push(cardArg(card, "family_id"));
  push(cardArg(card, "family"));
  push(cardArg(card, "family_name"));
  push(cardArg(card, "series"));
  push(cardArg(card, "collection"));
  push(cardArg(card, "pipeline_kind"));
  push(cardArg(card, "type_id"));

  // Compact episode frame as one chip (filterable), not each field alone.
  const epNo = cardArg(card, "episode_no") || cardArg(card, "episode");
  const epTotal = cardArg(card, "episode_total");
  if (epNo) {
    push(epTotal ? `ÉP ${epNo}/${epTotal}` : `ÉP ${epNo}`);
  }

  return out;
}

// cardHasAllTags is the AND-match used by the board label/tag filter.
export function cardHasAllTags(card: PipelineBoardCard, required: Set<string>): boolean {
  if (!required || required.size === 0) return true;
  const have = new Set(cardTags(card).map((t) => t.toLowerCase()));
  for (const t of required) {
    if (!have.has(t.toLowerCase())) return false;
  }
  return true;
}

// collectTagVocabulary is the union of every card's tags (sorted).
export function collectTagVocabulary(cards: PipelineBoardCard[]): string[] {
  const all = new Set<string>();
  for (const card of cards) {
    for (const t of cardTags(card)) all.add(t);
  }
  return Array.from(all).sort((a, b) => a.localeCompare(b));
}

// faceTags splits the full list into chips shown on the card + overflow count.
export function faceTags(card: PipelineBoardCard, limit = CARD_TAG_FACE_LIMIT): {
  shown: string[];
  more: number;
} {
  const all = cardTags(card);
  if (all.length <= limit) return { shown: all, more: 0 };
  return { shown: all.slice(0, limit), more: all.length - limit };
}

// looksLikePathTag rejects filesystem/URL-ish values. Allows short fractions
// like "ÉP 1/5" or "3/4" (at most one slash, no dots/backslashes).
function looksLikePathTag(t: string): boolean {
  if (t.includes("\\")) return true;
  const slashes = (t.match(/\//g) ?? []).length;
  if (slashes === 0) return false;
  if (slashes >= 2) return true;
  // Single slash: path if it has a dot segment or looks like dir/file.
  if (t.includes(".")) return true;
  // "ÉP 1/5" / "v2/final" ok; "assets/foo" has letters on both sides with
  // no digit-only side — still a path-ish label. Prefer: allow only when
  // at least one side is purely numeric (episode fractions).
  const [left, right] = t.split("/");
  const leftNum = /^\d+$/.test(left.trim()) || /\b\d+$/.test(left.trim());
  const rightNum = /^\d+$/.test(right.trim());
  return !(leftNum || rightNum);
}
