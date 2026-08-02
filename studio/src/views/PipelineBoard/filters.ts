// Client-side filtering for the /pipelines board, mirroring the /board
// backlog's semantics (useBoardColumns): case-insensitive substring search,
// exact bot match, labels combined with AND. Filtering stays client-side —
// the projection is already in memory and a network round-trip per keystroke
// would make the search feel laggy.

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import { cardHasAllTags, cardTags, collectTagVocabulary } from "./cardTags";
import {
  cardBlocked,
  cardReady,
  closedOutcome,
  compareBlockedLast,
  compareLaunchOrder,
  isKnownLane,
} from "./cardPredicates";

/** Which inventory tab is active (default: opened). */
export type InventoryTab = "opened" | "closed";

/** Sub-filters within the Opened tab. */
export type OpenedSubfilter = "all" | "ready" | "not_ready";

/** Sub-filters within the Closed tab. */
export type ClosedSubfilter = "all" | "success" | "failed";

/**
 * Dependency filter for the Opened tab. Three-way rather than a boolean so
 * both questions are askable: "what can I launch right now" (unblocked) and
 * "what do I need to unblock" (blocked). A boolean could only ever express
 * one of them, and inverting it would make "a filter is active" the state
 * the board loads in.
 */
export type DepsFilter = "all" | "unblocked" | "blocked";

/**
 * Inventory card ordering. Default is priority (matches the admission loop's
 * launch order: higher P first, ties oldest-first). Closed history often
 * prefers "updated", which operators can pick in the Sort control.
 */
export type InventorySortMode = "priority" | "updated" | "created";

export const INVENTORY_SORT_OPTIONS: {
  value: InventorySortMode;
  label: string;
}[] = [
  { value: "priority", label: "Priority" },
  { value: "updated", label: "Recently updated" },
  { value: "created", label: "Recently created" },
];

/**
 * Closed is HISTORY, not a queue. Priority is a launch-order key, and a
 * pipeline that already ran will never be launched by it again — ranking the
 * archive by P buries this morning's run under a months-old P9. So the Closed
 * tab reads chronologically, most recent first.
 *
 * effectiveSortMode is the ONE place that decision lives: the grid
 * (sortInventoryCards) and the Sort control (inventorySortOptions) both go
 * through it, so the select can never advertise an order the list is not in.
 * An explicit date choice is honoured as-is on both tabs.
 */
export function effectiveSortMode(
  mode: InventorySortMode = "priority",
  tab: InventoryTab = "opened",
): InventorySortMode {
  if (tab === "closed" && mode === "priority") return "updated";
  return mode;
}

/** Sort choices offered for a tab — Closed drops the meaningless ranking. */
export function inventorySortOptions(
  tab: InventoryTab = "opened",
): { value: InventorySortMode; label: string }[] {
  return tab === "closed"
    ? INVENTORY_SORT_OPTIONS.filter((o) => o.value !== "priority")
    : INVENTORY_SORT_OPTIONS;
}

export interface PipelineFilterState {
  query: string;
  bot: string;
  labels: Set<string>;
  /** bot_args.pipeline_kind exact match (empty = any). */
  pipelineKind: string;
  /** bot_args.family_id exact match (empty = any). */
  familyId: string;
  /** Dependency readiness chips (Opened tab only). */
  depsFilter: DepsFilter;
  /** Inventory tab: Opened (default) vs Closed. */
  inventoryTab: InventoryTab;
  /** Ready / not ready chips (Opened tab only). */
  openedSubfilter: OpenedSubfilter;
  /** Success / failed chips (Closed tab only). */
  closedSubfilter: ClosedSubfilter;
  /** How to order inventory cards (Opened + Closed tabs). */
  sortMode: InventorySortMode;
}

// Factory (not a shared constant): each call returns a fresh Set so a reset
// can never alias a previous selection.
export function emptyPipelineFilters(): PipelineFilterState {
  return {
    query: "",
    bot: "",
    labels: new Set(),
    pipelineKind: "",
    familyId: "",
    depsFilter: "all",
    inventoryTab: "opened",
    openedSubfilter: "all",
    closedSubfilter: "all",
    sortMode: "priority",
  };
}

export function pipelineFiltersActive(f: PipelineFilterState): boolean {
  return (
    (f.query ?? "").trim() !== "" ||
    (f.bot ?? "") !== "" ||
    (f.labels?.size ?? 0) > 0 ||
    (f.pipelineKind ?? "") !== "" ||
    (f.familyId ?? "") !== "" ||
    (f.depsFilter ?? "all") !== "all" ||
    (f.openedSubfilter ?? "all") !== "all" ||
    (f.closedSubfilter ?? "all") !== "all"
  );
}

// cardBotIdentity is the value the bot dropdown filters on: the bot id when
// the run carries one (catalog / bundle launches), else the workflow name.
// Loose .bot files (no manifest.yaml → not a bundle) produce runs with an
// empty bot_id; without the fallback those pipelines were simply absent from
// the dropdown.
export function cardBotIdentity(card: PipelineBoardCard): string {
  return card.bot_id || card.workflow_name || "";
}

// cardArg reads a bot_args / entry_input string key from the card projection.
export function cardArg(card: PipelineBoardCard, key: string): string {
  const v = card.entry_input?.[key];
  return typeof v === "string" ? v.trim() : "";
}

// collectFilterOptions derives the dropdown vocabularies from the cards
// actually on the board — including labels created on the fly by bots and
// content-derived tags (character, family_id, ÉP n/N, …).
export function collectFilterOptions(cards: PipelineBoardCard[]): {
  allBots: string[];
  allLabels: string[];
  allKinds: string[];
  allFamilies: string[];
} {
  const bots = new Set<string>();
  const kinds = new Set<string>();
  const families = new Set<string>();
  for (const card of cards) {
    const identity = cardBotIdentity(card);
    if (identity) bots.add(identity);
    const kind = cardArg(card, "pipeline_kind");
    if (kind) kinds.add(kind);
    const family = cardArg(card, "family_id");
    if (family) families.add(family);
  }
  return {
    allBots: Array.from(bots).sort(),
    // "Labels" filter is the full tag vocabulary (issue labels ∪ derived).
    allLabels: collectTagVocabulary(cards),
    allKinds: Array.from(kinds).sort(),
    allFamilies: Array.from(families).sort(),
  };
}

// cardMatchesRepo returns true when the card's forge identity resolves to the
// scoped repo full name. Task-backed cards carry the operator's connected
// `owner/repo`; run-only cards fall back to `project_path` which may be
// host-prefixed, so accept a "/<repo_full_name>" suffix as an alias.
export function cardMatchesRepo(
  card: PipelineBoardCard,
  repoFullName: string,
): boolean {
  const key = card.external?.repo;
  if (!key || !repoFullName) return false;
  return key === repoFullName || key.endsWith("/" + repoFullName);
}

// filterPipelineCards applies the active filters. Search matches the card's
// title, body, workflow name, run id, and issue id (case-insensitive);
// selected labels must ALL be present; bot is an exact match on the card's
// bot identity (bot_id, falling back to workflow_name — see cardBotIdentity).
// When `repoScope` is set the card must match it via cardMatchesRepo, unless
// `includeUnscoped` allows repo-less cards through as well.
// Lifecycle chips for the inventory section are applied separately (see
// filterInventoryCards) so in-progress is never hidden by "opened only".
export function filterPipelineCards(
  cards: PipelineBoardCard[],
  f: PipelineFilterState,
  repoScope: string | null = null,
  includeUnscoped = false,
): PipelineBoardCard[] {
  const q = (f.query ?? "").trim().toLowerCase();
  const bot = (f.bot ?? "").trim();
  const kind = (f.pipelineKind ?? "").trim();
  const family = (f.familyId ?? "").trim();
  const labels = f.labels ?? new Set<string>();
  const hasTextFilters =
    q !== "" || bot !== "" || labels.size > 0 || kind !== "" || family !== "";
  if (!hasTextFilters && !repoScope) return cards;
  return cards.filter((card) => {
    if (q) {
      const hay = [
        card.title,
        card.body ?? "",
        card.workflow_name ?? "",
        card.run_id ?? "",
        card.issue_id ?? "",
        cardArg(card, "input_path"),
        cardArg(card, "asset_id"),
        cardArg(card, "feature_id"),
        ...cardTags(card),
      ]
        .join("\t")
        .toLowerCase();
      if (!hay.includes(q)) return false;
    }
    if (bot && cardBotIdentity(card) !== bot) return false;
    if (labels.size > 0 && !cardHasAllTags(card, labels)) return false;
    if (kind && cardArg(card, "pipeline_kind") !== kind) return false;
    if (family && cardArg(card, "family_id") !== family) return false;
    // The dependency filter is deliberately NOT applied here. This function
    // narrows the WHOLE board, and running / needs-attention cards carry no
    // dependency fields at all (attachDeps only runs for ticket cards), so a
    // board-wide deps filter emptied the In-progress section as a side
    // effect. It belongs to the Opened tab — see filterInventoryCards.
    if (repoScope) {
      const hasRepo = !!card.external?.repo;
      if (!hasRepo) {
        if (!includeUnscoped) return false;
      } else if (!cardMatchesRepo(card, repoScope)) {
        return false;
      }
    }
    return true;
  });
}

/** Newest first by updated_at (fallback created_at). */
export function sortNewestFirst(cards: PipelineBoardCard[]): PipelineBoardCard[] {
  return [...cards].sort((a, b) => {
    const ta = Date.parse(a.updated_at || a.created_at || "") || 0;
    const tb = Date.parse(b.updated_at || b.created_at || "") || 0;
    if (tb !== ta) return tb - ta;
    // Stable tie-break: ascending id.
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

/**
 * Inventory ordering. "priority" matches the server's launch order (P desc,
 * then oldest-first) and sinks dependency-blocked cards below launchable
 * ones first, so the top of the list is always something the operator can
 * actually start. Date modes are newest-first. Does not mutate the input
 * array.
 *
 * Priority applies to the Opened queue only — on Closed it resolves to
 * chronology (see effectiveSortMode), which also makes the blocked-last
 * partition moot there: a done ticket still carries whatever blockers it had
 * (attachDeps runs for terminal task cards too), and reshuffling history for
 * them would be noise.
 */
export function sortInventoryCards(
  cards: PipelineBoardCard[],
  mode: InventorySortMode = "priority",
  tab: InventoryTab = "opened",
): PipelineBoardCard[] {
  const effective = effectiveSortMode(mode, tab);
  if (effective === "updated") return sortNewestFirst(cards);
  if (effective === "priority") {
    return [...cards].sort((a, b) => {
      const byBlocked = compareBlockedLast(a, b);
      if (byBlocked !== 0) return byBlocked;
      return compareLaunchOrder(a, b);
    });
  }
  // created — newest first
  return [...cards].sort((a, b) => {
    const ta = Date.parse(a.created_at || "") || 0;
    const tb = Date.parse(b.created_at || "") || 0;
    if (tb !== ta) return tb - ta;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

export function partitionPipelineCards(cards: PipelineBoardCard[]): {
  inProgress: PipelineBoardCard[];
  needsAttention: PipelineBoardCard[];
  inventory: PipelineBoardCard[];
} {
  const inProgress: PipelineBoardCard[] = [];
  const needsAttention: PipelineBoardCard[] = [];
  const inventory: PipelineBoardCard[] = [];
  for (const card of cards) {
    if (card.column_id === "in_progress") inProgress.push(card);
    else if (card.column_id === "needs_attention") needsAttention.push(card);
    else inventory.push(card);
  }
  return {
    inProgress: sortNewestFirst(inProgress),
    needsAttention: sortNewestFirst(needsAttention),
    inventory: sortNewestFirst(inventory),
  };
}

/**
 * inventoryLane maps a card to the tab that must show it. Anything this
 * build does not recognise falls into Closed rather than disappearing: a
 * newer server can add a lane, and an SPA bundle already in a browser tab
 * cannot be retro-fixed. Dropping such a card from BOTH tabs (the previous
 * behaviour) makes it invisible with no counter and no error.
 */
function inventoryLane(card: PipelineBoardCard): "opened" | "closed" | null {
  if (card.column_id === "opened") return "opened";
  if (card.column_id === "closed") return "closed";
  // In progress and needs attention have their own sections above.
  if (card.column_id === "in_progress" || card.column_id === "needs_attention") {
    return null;
  }
  return isKnownLane(card.column_id) ? null : "closed";
}

/** Apply inventory tab + subfilters to opened/closed cards (not in-progress). */
export function filterInventoryCards(
  cards: PipelineBoardCard[],
  f: Pick<
    PipelineFilterState,
    "inventoryTab" | "openedSubfilter" | "closedSubfilter" | "depsFilter"
  >,
): PipelineBoardCard[] {
  const tab = f.inventoryTab ?? "opened";
  if (tab === "opened") {
    const sub = f.openedSubfilter ?? "all";
    const deps = f.depsFilter ?? "all";
    return cards.filter((card) => {
      if (inventoryLane(card) !== "opened") return false;
      if (deps === "unblocked" && cardBlocked(card)) return false;
      if (deps === "blocked" && !cardBlocked(card)) return false;
      if (sub === "ready") return cardReady(card);
      if (sub === "not_ready") return !cardReady(card);
      return true;
    });
  }
  const sub = f.closedSubfilter ?? "all";
  return cards.filter((card) => {
    if (inventoryLane(card) !== "closed") return false;
    if (sub === "success") return closedOutcome(card) === "success";
    if (sub === "failed") return closedOutcome(card) === "failed";
    return true;
  });
}

/** Counts for tab badges (pre-subfilter, post text filters). */
export function inventoryTabCounts(cards: PipelineBoardCard[]): {
  opened: number;
  closed: number;
} {
  let opened = 0;
  let closed = 0;
  for (const card of cards) {
    const lane = inventoryLane(card);
    if (lane === "opened") opened++;
    else if (lane === "closed") closed++;
  }
  return { opened, closed };
}
