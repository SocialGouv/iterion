import { useEffect, useMemo } from "react";

import type { BotEntry } from "@/api/bots";
import { Select } from "@/components/ui/Select";
import { botVisual, canon } from "@/lib/personas";
import { useBotsStore } from "@/store/bots";

/** botFilterLabel renders a filter-dropdown option for a raw bot identity
 *  (a card's bot_id, or its workflow-name fallback): the catalog persona
 *  (emoji + display_name) when the identity resolves to a discovered bot,
 *  else the raw identity unchanged. Snake/kebab variants of one id resolve
 *  the same catalog entry (canon), so a loose run's `pipeline_board_demo`
 *  workflow name still finds the `pipeline-board-demo` bundle. */
export function botFilterLabel(
  identity: string,
  catalog: BotEntry[] | null | undefined,
): string {
  const key = canon(identity);
  const hit = (catalog ?? []).find((b) => canon(b.name) === key);
  const persona = hit?.display_name?.trim();
  if (!hit || !persona) return identity;
  return `${botVisual(hit).emoji} ${persona}`;
}

export interface BotFilterOption {
  value: string;
  label: string;
}

/** botFilterOptions maps the board's raw bot identities to dropdown
 *  options. The option VALUE stays the raw identity — filtering is an
 *  exact match on card data, only the label is humanized. Two raw
 *  identities can resolve the same persona (a bundle launch's bot_id
 *  `pipeline-board-demo` next to a loose run's workflow-name fallback
 *  `pipeline_board_demo`): each keeps its own option so neither
 *  population becomes unreachable, and colliding labels get the raw
 *  identity appended so the operator can tell them apart. */
export function botFilterOptions(
  allBots: string[],
  catalog: BotEntry[] | null | undefined,
): BotFilterOption[] {
  const options = allBots.map((value) => ({
    value,
    label: botFilterLabel(value, catalog),
  }));
  const labelCounts = new Map<string, number>();
  for (const o of options) {
    labelCounts.set(o.label, (labelCounts.get(o.label) ?? 0) + 1);
  }
  for (const o of options) {
    if ((labelCounts.get(o.label) ?? 0) > 1 && o.label !== o.value) {
      o.label = `${o.label} (${o.value})`;
    }
  }
  // Order by what the operator reads: the persona/identity text, not the
  // leading emoji token (raw-identity labels have no emoji to strip).
  return options.sort((a, b) =>
    a.label.replace(/^\S+\s/, "").localeCompare(b.label.replace(/^\S+\s/, "")),
  );
}

/** BotFilterSelect is the shared bot dropdown of the /board and /pipelines
 *  filter bars. It reads the shared bots store (fetching it on first
 *  mount) so raw slugs upgrade to persona names as soon as the catalog
 *  loads; identities without a catalog match keep rendering raw. */
export function BotFilterSelect({
  value,
  allBots,
  onChange,
  ariaLabel = "Filter by bot",
}: {
  value: string;
  /** Raw identities present on the board's cards (already deduped). */
  allBots: string[];
  onChange: (v: string) => void;
  ariaLabel?: string;
}) {
  const bots = useBotsStore((s) => s.bots);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    if (bots === null) void fetchBots();
  }, [bots, fetchBots]);

  const options = useMemo(() => botFilterOptions(allBots, bots), [allBots, bots]);

  return (
    <div className="w-auto">
      <Select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        aria-label={ariaLabel}
      >
        <option value="">All bots</option>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </Select>
    </div>
  );
}
