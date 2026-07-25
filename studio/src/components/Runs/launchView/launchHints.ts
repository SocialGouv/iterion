// Pure application of a bot's launch-form hints (manifest `launch:` block,
// surfaced on the bot entry as BotLaunchHints) onto the heuristic var
// buckets from varClassify. Covered by launchHints.test.ts — no React here.
//
// Hints are presentational only: they move fields between buckets (or drop
// them from rendering); requiredness/validation and the launch payload keep
// reading the full field list, so a hinted-primary var with a default stays
// optional and a hidden var still resolves to its declared default
// server-side.

import type { BotLaunchHints } from "@/api/bots";
import type { VarField } from "@/api/types";

import { classifyVar } from "./varClassify";

export interface LaunchVarBuckets {
  /** Always-visible inputs: hint-forced names first (in hinted order),
   *  then the heuristic primaries (required, no default) in declaration
   *  order. */
  primary: VarField[];
  /** Optional inputs with defaults — the "Bot options" disclosure. */
  advanced: VarField[];
  /** Runner-resolved `${PROJECT*_DIR}` defaults — read-only rows in the
   *  "Bot options" disclosure. */
  auto: VarField[];
  /** Names that made it into `primary` via the hint (post hidden-filter +
   *  dedupe). The form gives their string inputs prompt-style prominence. */
  hintedPrimary: ReadonlySet<string>;
}

/** applyLaunchHints buckets `fields` for progressive disclosure, folding in
 *  the bot's optional launch hints:
 *  - `hidden` names are removed from every bucket (never rendered) and win
 *    over a `primary` listing of the same name;
 *  - `primary` names are forced into the primary bucket in hinted order,
 *    ahead of the heuristic primaries (which keep their relative order);
 *    a name that is both hinted and heuristically primary appears once, at
 *    its hinted position;
 *  - names that match no declared field are ignored silently. */
export function applyLaunchHints(
  fields: VarField[],
  hints?: BotLaunchHints | null,
): LaunchVarBuckets {
  const hidden = new Set(hints?.hidden ?? []);
  const visible = fields.filter((f) => !hidden.has(f.name));
  const byName = new Map(visible.map((f) => [f.name, f]));

  const hintedPrimary = new Set<string>();
  const primary: VarField[] = [];
  for (const name of hints?.primary ?? []) {
    if (hintedPrimary.has(name)) continue; // repeated hint — first position wins
    const f = byName.get(name);
    if (!f) continue; // unknown or hidden — ignored silently
    hintedPrimary.add(name);
    primary.push(f);
  }

  const advanced: VarField[] = [];
  const auto: VarField[] = [];
  for (const f of visible) {
    if (hintedPrimary.has(f.name)) continue; // already placed at its hinted position
    switch (classifyVar(f)) {
      case "primary":
        primary.push(f);
        break;
      case "advanced":
        advanced.push(f);
        break;
      case "auto":
        auto.push(f);
        break;
    }
  }

  return { primary, advanced, auto, hintedPrimary };
}
