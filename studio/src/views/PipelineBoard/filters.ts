// Client-side filtering for the /pipelines board, mirroring the /board
// backlog's semantics (useBoardColumns): case-insensitive substring search,
// exact bot match, labels combined with AND. Filtering stays client-side —
// the projection is already in memory and a network round-trip per keystroke
// would make the search feel laggy.

import type { PipelineBoardCard } from "@/api/pipelineBoards";

export interface PipelineFilterState {
  query: string;
  bot: string;
  labels: Set<string>;
}

// Factory (not a shared constant): each call returns a fresh Set so a reset
// can never alias a previous selection.
export function emptyPipelineFilters(): PipelineFilterState {
  return { query: "", bot: "", labels: new Set() };
}

export function pipelineFiltersActive(f: PipelineFilterState): boolean {
  return f.query.trim() !== "" || f.bot !== "" || f.labels.size > 0;
}

// collectFilterOptions derives the dropdown vocabularies from the cards
// actually on the board — including labels created on the fly by bots.
export function collectFilterOptions(cards: PipelineBoardCard[]): {
  allBots: string[];
  allLabels: string[];
} {
  const bots = new Set<string>();
  const labels = new Set<string>();
  for (const card of cards) {
    if (card.bot_id) bots.add(card.bot_id);
    for (const l of card.labels ?? []) labels.add(l);
  }
  return {
    allBots: Array.from(bots).sort(),
    allLabels: Array.from(labels).sort(),
  };
}

// filterPipelineCards applies the active filters. Search matches the card's
// title, body, workflow name, run id, and issue id (case-insensitive);
// selected labels must ALL be present; bot is an exact match.
export function filterPipelineCards(
  cards: PipelineBoardCard[],
  f: PipelineFilterState,
): PipelineBoardCard[] {
  const q = f.query.trim().toLowerCase();
  const bot = f.bot.trim();
  if (!q && !bot && f.labels.size === 0) return cards;
  return cards.filter((card) => {
    if (q) {
      const hay = [
        card.title,
        card.body ?? "",
        card.workflow_name ?? "",
        card.run_id ?? "",
        card.issue_id ?? "",
      ]
        .join("\t")
        .toLowerCase();
      if (!hay.includes(q)) return false;
    }
    if (bot && card.bot_id !== bot) return false;
    if (f.labels.size > 0) {
      const have = new Set(card.labels ?? []);
      for (const l of f.labels) {
        if (!have.has(l)) return false;
      }
    }
    return true;
  });
}
