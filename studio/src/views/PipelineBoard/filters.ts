// Client-side filtering for the /pipelines board, mirroring the /board
// backlog's semantics (useBoardColumns): case-insensitive substring search,
// exact bot match, labels combined with AND. Filtering stays client-side —
// the projection is already in memory and a network round-trip per keystroke
// would make the search feel laggy.

import type { PipelineBoardCard } from "@/api/pipelineBoards";

import { cardHasAllTags, cardTags, collectTagVocabulary } from "./cardTags";
import { cardReady, closedOutcome } from "./columnFilters";

/** Which inventory tab is active (default: opened). */
export type InventoryTab = "opened" | "closed";

/** Sub-filters within the Opened tab. */
export type OpenedSubfilter = "all" | "ready" | "not_ready";

/** Sub-filters within the Closed tab. */
export type ClosedSubfilter = "all" | "success" | "failed";

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

export interface PipelineFilterState {
  query: string;
  bot: string;
  labels: Set<string>;
  /** bot_args.pipeline_kind exact match (empty = any). */
  pipelineKind: string;
  /** bot_args.family_id exact match (empty = any). */
  familyId: string;
  /** Only cards with open hard blockers or issue_state waiting_deps. */
  waitingDepsOnly: boolean;
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
    waitingDepsOnly: false,
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
    !!f.waitingDepsOnly ||
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
    q !== "" ||
    bot !== "" ||
    labels.size > 0 ||
    kind !== "" ||
    family !== "" ||
    !!f.waitingDepsOnly;
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
    if (f.waitingDepsOnly) {
      const open = card.open_blocker_count ?? 0;
      const waiting =
        card.issue_state === "waiting_deps" ||
        card.launch_blocked_reason === "waiting_deps" ||
        card.launch_blocked_reason === "open_blockers" ||
        open > 0;
      if (!waiting) return false;
    }
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
 * Inventory ordering. "priority" matches server sortReadyTickets /
 * queueSummary.sortLaunchOrder (P desc, then oldest-first). Date modes are
 * newest-first. Does not mutate the input array.
 */
export function sortInventoryCards(
  cards: PipelineBoardCard[],
  mode: InventorySortMode = "priority",
): PipelineBoardCard[] {
  if (mode === "updated") return sortNewestFirst(cards);
  return [...cards].sort((a, b) => {
    if (mode === "priority") {
      const pa = a.priority ?? 0;
      const pb = b.priority ?? 0;
      if (pa !== pb) return pb - pa;
      const ta = Date.parse(a.created_at || "") || 0;
      const tb = Date.parse(b.created_at || "") || 0;
      if (ta !== tb) return ta - tb;
      return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
    }
    // created — newest first
    const ta = Date.parse(a.created_at || "") || 0;
    const tb = Date.parse(b.created_at || "") || 0;
    if (tb !== ta) return tb - ta;
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  });
}

export function partitionPipelineCards(cards: PipelineBoardCard[]): {
  inProgress: PipelineBoardCard[];
  inventory: PipelineBoardCard[];
} {
  const inProgress: PipelineBoardCard[] = [];
  const inventory: PipelineBoardCard[] = [];
  for (const card of cards) {
    if (card.column_id === "in_progress") inProgress.push(card);
    else inventory.push(card);
  }
  return {
    inProgress: sortNewestFirst(inProgress),
    inventory: sortNewestFirst(inventory),
  };
}

/** Apply inventory tab + subfilter to opened/closed cards (not in-progress). */
export function filterInventoryCards(
  cards: PipelineBoardCard[],
  f: Pick<PipelineFilterState, "inventoryTab" | "openedSubfilter" | "closedSubfilter">,
): PipelineBoardCard[] {
  const tab = f.inventoryTab ?? "opened";
  if (tab === "opened") {
    const sub = f.openedSubfilter ?? "all";
    return cards.filter((card) => {
      if (card.column_id !== "opened") return false;
      if (sub === "ready") return cardReady(card);
      if (sub === "not_ready") return !cardReady(card);
      return true;
    });
  }
  const sub = f.closedSubfilter ?? "all";
  return cards.filter((card) => {
    if (card.column_id !== "closed") return false;
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
    if (card.column_id === "opened") opened++;
    else if (card.column_id === "closed") closed++;
  }
  return { opened, closed };
}
