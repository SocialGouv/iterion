import { useEffect, useState, type ReactNode } from "react";
import { Link, useLocation } from "wouter";

import type {
  PipelineBoard,
  PipelineBoardCard as PipelineBoardCardDTO,
} from "@/api/pipelineBoards";
import { DotsHorizontalIcon } from "@radix-ui/react-icons";

import {
  bulkDeletePipelineTasks,
  bulkReadyPipelineTasks,
  deletePipelineTask,
  launchPipelineTask,
  markPipelineTaskReady,
  resetPipelineTask,
} from "@/api/pipelineBoards";
import { cancelRun, pauseRun } from "@/api/runs";
import type { UnifiedStatus } from "@/components/Runs/runStatusClasses";
import {
  Badge,
  Button,
  Card,
  Checkbox,
  DropdownMenu,
  DropdownMenuItem,
  IconButton,
  InlineBanner,
  StatusBadge,
} from "@/components/ui";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
import { formatRelative } from "@/lib/format";
import { useBotsStore } from "@/store/bots";
import { useUIStore } from "@/store/ui";

import { cardRoutePath } from "./cardRoute";
import {
  canDeleteTicket,
  canLaunchNow,
  canMarkReady,
  canPauseRun,
  canResetTicket,
  canResumeRun,
  canStopRun,
  canUnmarkReady,
  isTicketEditable,
} from "./cardCapabilities";
import { cardReady, closedOutcome } from "./columnFilters";
import {
  filterInventoryCards,
  inventoryTabCounts,
  partitionPipelineCards,
  sortInventoryCards,
  type PipelineFilterState,
} from "./filters";
import { PipelineFilters } from "./PipelineFilters";
import {
  isCardSelectable,
  resolveMenuItems,
  resolvePrimaryAction,
  type MenuItemKind,
  type PrimaryKind,
} from "./primaryAction";
import { QueueBanner } from "./QueueBanner";
import { faceTags } from "./cardTags";
import { formatChildrenSummary } from "./planGroups";
import { hasOpenDeps } from "./queueSummary";
import { resumePipelineRun } from "./resumePipelineRun";

// Re-export capabilities for existing tests.
export {
  canDeleteTicket,
  canLaunchNow,
  canMarkReady,
  canPauseRun,
  canResetTicket,
  canResumeRun,
  canStopRun,
  canUnmarkReady,
  isTicketEditable,
};

interface Props {
  board: PipelineBoard;
  onRefetch: () => void;
  onEditTask?: (card: PipelineBoardCardDTO) => void;
  onOpenCard?: (card: PipelineBoardCardDTO, focus?: OpenCardFocus) => void;
  /** Full board cards (unfiltered by inventory tab) — for queue banner. */
  allCardsForQueue?: PipelineBoardCardDTO[];
  filters?: PipelineFilterState;
  onFiltersChange?: (next: PipelineFilterState) => void;
  onFiltersReset?: () => void;
  filterOptions?: {
    allBots: string[];
    allLabels: string[];
    allKinds: string[];
    allFamilies: string[];
  };
  /** Repo-first scoping (cloud): forwarded to the filter bar's
   *  "Include unscoped" affordance. */
  repoScope?: string | null;
  includeUnscoped?: boolean;
  onIncludeUnscopedChange?: (v: boolean) => void;
}

// isInteractiveClick: card body always opens the drawer. Only the footer
// (CTA / ⋯ / run link) and the multi-select checkbox may swallow the click.
// Mark those regions with data-card-footer / data-no-card-open.
function isInteractiveClick(e: React.MouseEvent): boolean {
  const boundary = e.currentTarget;
  let node = e.target as HTMLElement | null;
  while (node && node !== boundary) {
    if (
      node.hasAttribute("data-card-footer") ||
      node.hasAttribute("data-no-card-open")
    ) {
      return true;
    }
    node = node.parentElement;
  }
  return false;
}

const KNOWN_STATUSES = new Set<UnifiedStatus>([
  "running",
  "paused_waiting_human",
  "paused_operator",
  "finished",
  "failed",
  "failed_resumable",
  "cancelled",
  "queued",
  "skipped",
  "none",
]);

function isKnownStatus(status: string): status is UnifiedStatus {
  return KNOWN_STATUSES.has(status as UnifiedStatus);
}

function humanizeToken(value: string): string {
  const label = value.replace(/[_-]+/g, " ").trim();
  return label ? label.charAt(0).toUpperCase() + label.slice(1) : value;
}

// moveTicket flips a ticket's ready state (true → staged for the launch loop;
// false → back to being-prepared), then refetches the board.
export async function moveTicket(
  issueId: string,
  ready: boolean,
  onDone: () => void,
): Promise<void> {
  await markPipelineTaskReady(issueId, ready);
  onDone();
}

export type OpenCardFocus = "default" | "deps" | "review";

// Custom MIME type for the Opened → In progress drag. Using a bespoke type
// (not text/plain) means the In-progress zone only lights up for a ticket
// drag, and a stray text selection dropped on the board does nothing.
export const LAUNCH_DRAG_TYPE = "application/x-iterion-ticket";

// botEditorPath maps a card's bot_id to the workspace-relative main.bot the
// editor opens (/editor?file=…), using the catalog's server-relativised
// rel_path — the same derivation the Catalog dialog and Launch view use.
// Null for a loose .bot (no bundle) or a bot the catalog can't resolve:
// there is nothing stable to open, so the menu entry is simply not offered.
export function botEditorPath(
  botID: string | undefined,
  bots: { name: string; rel_path?: string; is_bundle?: boolean }[] | null,
): string | null {
  const id = (botID ?? "").trim();
  if (!id || !bots) return null;
  const entry = bots.find((b) => b.name === id);
  if (!entry || !entry.is_bundle || !entry.rel_path) return null;
  return `${entry.rel_path}/main.bot`;
}

// PipelineCardActions bundles run controls + ticket lifecycle.
export interface PipelineCardActions {
  onPause: (card: PipelineBoardCardDTO) => void;
  onResume: (card: PipelineBoardCardDTO) => void;
  onStop: (card: PipelineBoardCardDTO) => void;
  onReset: (card: PipelineBoardCardDTO) => void;
  onDelete: (card: PipelineBoardCardDTO) => void;
}

export function PipelineColumns({
  board,
  onRefetch,
  onEditTask,
  onOpenCard,
  allCardsForQueue,
  filters,
  onFiltersChange,
  onFiltersReset,
  filterOptions,
  repoScope,
  includeUnscoped,
  onIncludeUnscopedChange,
}: Props) {
  const { cards } = board;
  const [actionError, setActionError] = useState<string | null>(null);
  const [selectedIds, setSelectedIds] = useState<Set<string>>(() => new Set());
  const [bulkBusy, setBulkBusy] = useState(false);
  // Drag & drop is scoped to ONE transition: Opened → In progress, the
  // operator's explicit "run this now" override of the priority queue.
  // Every other card position stays server-derived (this board has no
  // free-form drag).
  const [dropActive, setDropActive] = useState(false);
  // True while a ticket is in flight, so the In-progress zone can advertise
  // itself as a target before the pointer even reaches it.
  const [dragging, setDragging] = useState(false);
  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog: confirmDialog } = useConfirm();
  // Warm the catalog so a card's "Edit bot" entry can resolve its main.bot.
  // The store dedupes: this is a no-op once any view has loaded the bots.
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    void fetchBots();
  }, [fetchBots]);

  const onMoveTicket = async (issueId: string, ready: boolean) => {
    setActionError(null);
    try {
      await moveTicket(issueId, ready, onRefetch);
    } catch (e) {
      setActionError(errorMessage(e));
    }
  };

  const runAction = async (fn: () => Promise<unknown>) => {
    setActionError(null);
    try {
      await fn();
      onRefetch();
    } catch (e) {
      setActionError(errorMessage(e));
    }
  };

  const actions: PipelineCardActions = {
    // Pause / Resume are reversible — no confirm friction.
    onPause: (card) => void runAction(() => pauseRun(card.run_id as string)),
    onResume: (card) =>
      void runAction(() =>
        resumePipelineRun(card.run_id as string, () =>
          confirm({
            title: "Workflow changed since this run started",
            message:
              "Resume from the saved checkpoint using the current workflow source?",
            confirmLabel: "Resume with updated workflow",
          }),
        ),
      ),
    onStop: async (card) => {
      if (
        !(await confirm({
          title: "Stop this pipeline?",
          message:
            "The run is cancelled at the next safe boundary and lands in Closed as failed, with its partial work preserved.",
          confirmLabel: "Stop",
          confirmVariant: "danger",
        }))
      )
        return;
      await runAction(() => cancelRun(card.run_id as string));
    },
    onReset: async (card) => {
      if (
        !(await confirm({
          title: "Reset this ticket?",
          message:
            "The current run (and its sub-runs) are cancelled, then the ticket is restaged to Ready and starts over from zero.",
          confirmLabel: "Reset",
          confirmVariant: "danger",
        }))
      )
        return;
      await runAction(() => resetPipelineTask(card.issue_id as string));
    },
    onDelete: async (card) => {
      if (
        !(await confirm({
          title: "Delete this ticket?",
          message: "This removes it from the board and cannot be undone.",
          confirmLabel: "Delete",
          confirmVariant: "danger",
        }))
      )
        return;
      await runAction(() => deletePipelineTask(card.issue_id as string));
    },
  };

  const onLaunchNow = async (issueId: string) => {
    setDropActive(false);
    await runAction(() => launchPipelineTask(issueId));
  };

  const { inProgress, inventory } = partitionPipelineCards(cards);
  const tab = filters?.inventoryTab ?? "opened";
  const tabCounts = inventoryTabCounts(inventory);
  const inventoryFiltered = filters
    ? filterInventoryCards(inventory, filters)
    : inventory;
  // Opened defaults to priority order (same as the admission queue); Closed
  // history and operators who prefer recency can switch via Sort.
  const inventoryVisible = sortInventoryCards(
    inventoryFiltered,
    filters?.sortMode ?? "priority",
  );
  const tabTotal = tab === "opened" ? tabCounts.opened : tabCounts.closed;

  const selectableVisible = inventoryVisible.filter(isCardSelectable);
  const selectedOnPage = selectableVisible.filter((c) =>
    selectedIds.has(c.issue_id as string),
  );

  const toggleSelect = (issueId: string, on: boolean) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (on) next.add(issueId);
      else next.delete(issueId);
      return next;
    });
  };

  const selectAllVisible = () => {
    setSelectedIds(new Set(selectableVisible.map((c) => c.issue_id as string)));
  };

  const clearSelection = () => setSelectedIds(new Set());

  const onBulkReady = async () => {
    const ids = [...selectedIds];
    if (ids.length === 0) return;
    setBulkBusy(true);
    setActionError(null);
    try {
      const res = await bulkReadyPipelineTasks({ ids, only_satisfied: true });
      const nReady = res.ready?.length ?? 0;
      const nWait = res.waiting_deps?.length ?? 0;
      const nSkip = res.skipped?.length ?? 0;
      addToast(
        `Bulk ready: ${nReady} ready` +
          (nWait ? `, ${nWait} waiting on deps` : "") +
          (nSkip ? `, ${nSkip} skipped` : ""),
        nReady > 0 ? "success" : "info",
      );
      clearSelection();
      onRefetch();
    } catch (e) {
      setActionError(errorMessage(e));
    } finally {
      setBulkBusy(false);
    }
  };

  const onBulkDelete = async () => {
    const ids = [...selectedIds];
    if (ids.length === 0) return;
    if (
      !(await confirm({
        title: `Delete ${ids.length} ticket${ids.length === 1 ? "" : "s"}?`,
        message:
          "Removes the selected tickets from the board. Past runs stay in the run store. Tickets with an active run are skipped.",
        confirmLabel: "Delete",
        confirmVariant: "danger",
      }))
    ) {
      return;
    }
    setBulkBusy(true);
    setActionError(null);
    try {
      const res = await bulkDeletePipelineTasks({ ids });
      const nDel = res.deleted?.length ?? 0;
      const nSkip = res.skipped?.length ?? 0;
      addToast(
        `Bulk delete: ${nDel} removed` +
          (nSkip ? `, ${nSkip} skipped` : ""),
        nDel > 0 ? "success" : "info",
      );
      clearSelection();
      onRefetch();
    } catch (e) {
      setActionError(errorMessage(e));
    } finally {
      setBulkBusy(false);
    }
  };

  const queueCards = allCardsForQueue ?? cards;

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
      {confirmDialog}
      {actionError && (
        <div className="px-4 pt-2">
          <InlineBanner tone="danger" layout="inline">
            {actionError}
          </InlineBanner>
        </div>
      )}
      <div
        className="min-h-0 flex-1 space-y-6 overflow-y-auto px-4 pb-6"
        role="region"
        aria-label="Pipeline cards"
      >
        <div
          onDragOver={(e) => {
            if (!e.dataTransfer.types.includes(LAUNCH_DRAG_TYPE)) return;
            e.preventDefault();
            e.dataTransfer.dropEffect = "move";
            setDropActive(true);
          }}
          onDragLeave={(e) => {
            // Only clear when the pointer truly left the zone, not when it
            // crosses into a child element.
            if (e.currentTarget.contains(e.relatedTarget as Node)) return;
            setDropActive(false);
          }}
          onDrop={(e) => {
            const issueId = e.dataTransfer.getData(LAUNCH_DRAG_TYPE);
            if (!issueId) return;
            e.preventDefault();
            void onLaunchNow(issueId);
          }}
          className={`rounded-lg transition-colors ${
            dropActive
              ? "ring-2 ring-accent/60 bg-accent-soft/20"
              : dragging
                ? "ring-1 ring-dashed ring-accent/40"
                : ""
          }`}
          data-launch-dropzone
        >
          <CardSection
            id="in-progress"
            title="In progress"
            subtitle={
              dropActive || dragging
                ? "Drop a ticket here to launch it now — jumps the priority queue"
                : "Live pipelines — concurrency-capped"
            }
            accent="bg-info"
            count={inProgress.length}
            empty={
              dropActive
                ? "Drop here to start this ticket now."
                : "Nothing running right now."
            }
          >
            <CardGrid
              cards={inProgress}
              onMoveTicket={onMoveTicket}
              actions={actions}
              onEditTask={onEditTask}
              onOpenCard={onOpenCard}
            />
          </CardSection>
        </div>

        <QueueBanner
          cards={queueCards}
          concurrency={board.concurrency}
          onOpenCard={(c) => onOpenCard?.(c)}
        />

        <section aria-labelledby="pipeline-inventory-heading" className="space-y-3">
          <div className="flex flex-wrap items-end justify-between gap-2">
            <div>
              <div className="flex items-center gap-2">
                <span className="h-0.5 w-6 rounded bg-fg-subtle" aria-hidden />
                <h2
                  id="pipeline-inventory-heading"
                  className="text-xs font-semibold text-fg-default"
                >
                  Inventory
                </h2>
              </div>
              <p className="mt-0.5 text-micro text-fg-muted">
                Opened queue and closed history as tabs — default sort is
                priority (higher first, ties oldest).
              </p>
            </div>
          </div>

          {filters && onFiltersChange && onFiltersReset && filterOptions && (
            <PipelineFilters
              filters={filters}
              allBots={filterOptions.allBots}
              allLabels={filterOptions.allLabels}
              allKinds={filterOptions.allKinds}
              allFamilies={filterOptions.allFamilies}
              total={tabTotal}
              filtered={inventoryVisible.length}
              onChange={onFiltersChange}
              onReset={onFiltersReset}
              showInventoryChrome
              tabCounts={tabCounts}
              repoScope={repoScope}
              includeUnscoped={includeUnscoped}
              onIncludeUnscopedChange={onIncludeUnscopedChange}
            />
          )}

          {tab === "opened" && selectableVisible.length > 0 && (
            <div className="flex flex-wrap items-center gap-2 text-xs">
              <Button
                variant="ghost"
                size="sm"
                onClick={selectAllVisible}
                disabled={bulkBusy}
              >
                Select all visible ({selectableVisible.length})
              </Button>
              {selectedIds.size > 0 && (
                <>
                  <Button
                    variant="primary"
                    size="sm"
                    loading={bulkBusy}
                    onClick={() => void onBulkReady()}
                  >
                    Ready selected ({selectedOnPage.length || selectedIds.size})
                  </Button>
                  <Button
                    variant="danger"
                    size="sm"
                    loading={bulkBusy}
                    onClick={() => void onBulkDelete()}
                  >
                    Delete selected ({selectedOnPage.length || selectedIds.size})
                  </Button>
                  <Button variant="ghost" size="sm" onClick={clearSelection}>
                    Clear selection
                  </Button>
                </>
              )}
            </div>
          )}

          {inventoryVisible.length === 0 ? (
            <div className="flex min-h-24 items-center justify-center rounded-lg border border-dashed border-border-default px-3 text-center text-micro text-fg-subtle">
              {tab === "opened"
                ? "No opened tickets match these filters."
                : "No closed pipelines match these filters."}
            </div>
          ) : (
            <CardGrid
              cards={inventoryVisible}
              onMoveTicket={onMoveTicket}
              actions={actions}
              onEditTask={onEditTask}
              onOpenCard={onOpenCard}
              selectedIds={selectedIds}
              onToggleSelect={tab === "opened" ? toggleSelect : undefined}
              // Only Opened tickets can be launched now; a Closed card has
              // already run.
              draggableToLaunch={tab === "opened"}
              onDragStateChange={setDragging}
            />
          )}
        </section>
      </div>
    </div>
  );
}

function CardSection({
  id,
  title,
  subtitle,
  accent,
  count,
  empty,
  children,
}: {
  id: string;
  title: string;
  subtitle: string;
  accent: string;
  count: number;
  empty: string;
  children: ReactNode;
}) {
  return (
    <section aria-labelledby={`pipeline-section-${id}`} className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 border-b border-border-default pb-2">
        <span className={`h-0.5 w-6 rounded ${accent}`} aria-hidden />
        <h2
          id={`pipeline-section-${id}`}
          className="text-xs font-semibold text-fg-default"
        >
          {title}
        </h2>
        <Badge variant="neutral">{count}</Badge>
        <span className="text-micro text-fg-muted">{subtitle}</span>
      </div>
      {count === 0 ? (
        <div className="flex min-h-20 items-center justify-center rounded-lg border border-dashed border-border-default px-3 text-center text-micro text-fg-subtle">
          {empty}
        </div>
      ) : (
        children
      )}
    </section>
  );
}

function CardGrid({
  cards,
  onMoveTicket,
  actions,
  onEditTask,
  onOpenCard,
  selectedIds,
  onToggleSelect,
  draggableToLaunch,
  onDragStateChange,
}: {
  cards: PipelineBoardCardDTO[];
  onMoveTicket: (issueId: string, ready: boolean) => void;
  actions?: PipelineCardActions;
  onEditTask?: (card: PipelineBoardCardDTO) => void;
  onOpenCard?: (card: PipelineBoardCardDTO, focus?: OpenCardFocus) => void;
  selectedIds?: Set<string>;
  onToggleSelect?: (issueId: string, on: boolean) => void;
  /** Enables the Opened → In progress launch drag on eligible cards. */
  draggableToLaunch?: boolean;
  onDragStateChange?: (dragging: boolean) => void;
}) {
  // auto-rows:1fr is not needed — grid items stretch to the tallest card
  // in each row; PipelineCard uses h-full + flex so shorter siblings match.
  return (
    <div className="grid grid-cols-[repeat(auto-fill,minmax(16rem,1fr))] items-stretch gap-3">
      {cards.map((card) => (
        <PipelineCard
          key={card.id}
          card={card}
          onMoveTicket={onMoveTicket}
          actions={actions}
          onEditTask={onEditTask}
          onOpenCard={onOpenCard}
          selected={
            !!card.issue_id && !!selectedIds?.has(card.issue_id)
          }
          onToggleSelect={
            onToggleSelect && isCardSelectable(card)
              ? (on) => onToggleSelect(card.issue_id as string, on)
              : undefined
          }
          {...(draggableToLaunch && canLaunchNow(card)
            ? { launchDrag: { onDragStateChange } }
            : {})}
        />
      ))}
    </div>
  );
}

interface CardProps {
  card: PipelineBoardCardDTO;
  onMoveTicket: (issueId: string, ready: boolean) => void;
  actions?: PipelineCardActions;
  onEditTask?: (card: PipelineBoardCardDTO) => void;
  onOpenCard?: (card: PipelineBoardCardDTO, focus?: OpenCardFocus) => void;
  selected?: boolean;
  onToggleSelect?: (on: boolean) => void;
  /** Present when this card may be dragged into In progress to launch now. */
  launchDrag?: { onDragStateChange?: (dragging: boolean) => void };
}

// PipelineCard: lean status + primary CTA + ⋯ menu. Body click opens drawer.
export function PipelineCard({
  card,
  onMoveTicket,
  actions,
  onEditTask,
  onOpenCard,
  selected,
  onToggleSelect,
  launchDrag,
}: CardProps) {
  const timestamp = card.updated_at || card.created_at;
  const openable = !!onOpenCard;
  const primary = resolvePrimaryAction(card);
  const [, setLocation] = useLocation();
  const tags = faceTags(card);
  const bots = useBotsStore((s) => s.bots);
  const botFile = botEditorPath(card.bot_id, bots);
  // The bot entry is only offered when the catalog can point at a real
  // main.bot — a loose .bot file has nothing to open.
  const menu = resolveMenuItems(card, primary.kind).filter(
    (item) => item.kind !== "edit_bot" || botFile !== null,
  );

  const runAction = (kind: PrimaryKind | MenuItemKind) => {
    switch (kind) {
      case "mark_ready":
      case "retry":
        if (card.issue_id) void onMoveTicket(card.issue_id, true);
        break;
      case "unmark_ready":
        if (card.issue_id) void onMoveTicket(card.issue_id, false);
        break;
      case "view_deps":
        onOpenCard?.(card, "deps");
        break;
      case "answer_review":
        onOpenCard?.(card, "review");
        break;
      case "details":
        onOpenCard?.(card);
        break;
      case "edit":
        onEditTask?.(card);
        break;
      case "delete":
        actions?.onDelete(card);
        break;
      case "pause":
        actions?.onPause(card);
        break;
      case "resume":
        actions?.onResume(card);
        break;
      case "reset":
        actions?.onReset(card);
        break;
      case "stop":
        actions?.onStop(card);
        break;
      case "open_run":
        if (card.run_id) setLocation(`/runs/${encodeURIComponent(card.run_id)}`);
        break;
      case "full_page":
        setLocation(cardRoutePath(card));
        break;
      case "edit_bot":
        if (botFile) setLocation(`/editor?file=${encodeURIComponent(botFile)}`);
        break;
      default:
        break;
    }
  };

  return (
    <Card
      className={`flex h-full min-h-0 flex-col gap-2 p-3 ${openable ? "cursor-pointer" : ""} ${
        selected ? "ring-2 ring-accent/50 border-accent" : ""
      }`}
      interactive={openable}
      data-card-id={card.id}
      {...(launchDrag && card.issue_id
        ? {
            draggable: true,
            onDragStart: (e: React.DragEvent) => {
              e.dataTransfer.setData(LAUNCH_DRAG_TYPE, card.issue_id as string);
              e.dataTransfer.effectAllowed = "move";
              launchDrag.onDragStateChange?.(true);
            },
            onDragEnd: () => launchDrag.onDragStateChange?.(false),
            title: "Drag onto In progress to start now (skips the queue)",
          }
        : {})}
      role="article"
      aria-label={`${card.title}, ${humanizeToken(card.kind)}`}
      onClick={
        openable
          ? (e) => {
              if (isInteractiveClick(e)) return;
              e.stopPropagation();
              onOpenCard?.(card);
            }
          : undefined
      }
    >
      {/* Child tie: the campaign this ticket was spawned by, sitting above
          the title. Cards are ungrouped and independently ordered now, so
          this line is the ONLY thing carrying the parent relation on the
          face — deliberately typographic, with no card-level chrome. */}
      {card.parent_title && (
        <div className="-mb-0.5 flex min-w-0 items-center gap-1 text-micro text-accent-text">
          <span aria-hidden>↳</span>
          <span
            className="truncate"
            title={`Spawned by ${card.parent_title}`}
          >
            {card.parent_title}
          </span>
        </div>
      )}

      <div className="flex min-w-0 items-start gap-2">
        {onToggleSelect && (
          <div data-no-card-open className="pt-0.5" onClick={(e) => e.stopPropagation()}>
            <Checkbox
              checked={!!selected}
              onChange={(e) => onToggleSelect(e.target.checked)}
              aria-label={`Select ${card.title}`}
            />
          </div>
        )}
        <div className="min-w-0 flex-1">
          <CardTitle card={card} />
        </div>
      </div>

      {/* Body grows so cards in a grid row share one height; overflow stays clipped. */}
      <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-2 overflow-hidden">
        {card.column_id === "opened" && <TodoStatus card={card} />}
        {card.column_id === "in_progress" && <InProgressStatus card={card} />}
        {card.column_id === "closed" && <ClosedStatus card={card} />}

        {/* Parent side of the relation. The child side is the ↳ line above
            the title — repeating it here as "Plan: <parent>" was redundant. */}
        {(card.role === "planner" ||
          (card.children_summary && card.children_summary.total > 0)) && (
          <div className="flex min-w-0 flex-wrap items-center gap-1 text-micro text-fg-muted">
            <Badge variant="info" title="Planner — spawns child tickets">
              Plan
            </Badge>
          </div>
        )}

        {card.children_summary && card.children_summary.total > 0 && (
          <ChildrenProgressBar
            closed={
              (card.children_summary.done ?? 0) +
              (card.children_summary.failed ?? 0)
            }
            total={card.children_summary.total}
            detail={formatChildrenSummary(card.children_summary)}
          />
        )}

        {tags.shown.length > 0 && (
          <CardTags shown={tags.shown} more={tags.more} />
        )}

        <DepsPreview card={card} />
      </div>

      <div
        data-card-footer
        className="mt-auto flex min-w-0 shrink-0 items-center gap-2 border-t border-border-default pt-2 text-micro"
        onClick={(e) => e.stopPropagation()}
      >
        {card.run_id ? (
          <Link
            href={`/runs/${encodeURIComponent(card.run_id)}`}
            className="shrink-0 font-mono text-accent-text hover:underline"
            title={`Open run ${card.run_id}`}
            aria-label={`Open run ${card.run_id} in the run console`}
          >
            {card.run_id.slice(0, 12)}
          </Link>
        ) : (
          <span className="shrink-0 text-fg-subtle">Not started</span>
        )}
        {timestamp && (
          <span className="min-w-0 truncate text-fg-subtle" title={timestamp}>
            {formatRelative(timestamp)}
          </span>
        )}
        <span className="ml-auto flex shrink-0 items-center gap-1">
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              runAction(primary.kind);
            }}
            className={`rounded-md px-2 py-0.5 font-medium hover:underline ${
              primary.danger
                ? "text-danger-fg"
                : "bg-accent-soft text-accent-text"
            }`}
            aria-label={`${primary.label}: ${card.title}`}
          >
            {primary.label}
          </button>
          {menu.length > 0 && (
            <div onClick={(e) => e.stopPropagation()}>
              <DropdownMenu
                align="end"
                trigger={
                  <IconButton
                    label={`More actions for ${card.title}`}
                    size="sm"
                    variant="ghost"
                  >
                    <DotsHorizontalIcon />
                  </IconButton>
                }
              >
                {menu.map((item) => (
                  <DropdownMenuItem
                    key={item.kind}
                    className={item.danger ? "text-danger-fg" : undefined}
                    onSelect={() => runAction(item.kind)}
                  >
                    {item.label}
                  </DropdownMenuItem>
                ))}
              </DropdownMenu>
            </div>
          )}
        </span>
      </div>
    </Card>
  );
}

// DepsPreview is display-only — body click opens the drawer (Dependencies section).
function DepsPreview({ card }: { card: PipelineBoardCardDTO }) {
  if (!hasOpenDeps(card)) return null;
  const open =
    card.blockers?.filter((b) => !b.satisfied).slice(0, 2) ?? [];
  const more = Math.max(0, (card.open_blocker_count ?? open.length) - open.length);
  if (open.length === 0 && (card.open_blocker_count ?? 0) === 0) {
    return (
      <div className="w-full truncate rounded-md border border-warning/30 bg-warning-soft/40 px-2 py-1 text-left text-micro text-warning-fg">
        Waiting on deps — open card for graph
      </div>
    );
  }
  return (
    <div className="w-full min-w-0 space-y-0.5 rounded-md border border-warning/30 bg-warning-soft/40 px-2 py-1.5 text-left">
      <div className="text-micro font-medium text-warning-fg">
        Blocked by {card.open_blocker_count ?? open.length}
      </div>
      <ul className="min-w-0 space-y-0.5 text-micro text-fg-muted">
        {open.map((b) => (
          <li key={b.id} className="truncate" title={b.title || b.id}>
            · {b.title || b.id}
            {b.state ? (
              <span className="text-fg-subtle"> ({b.state})</span>
            ) : (
              <span className="text-danger-fg"> (missing)</span>
            )}
          </li>
        ))}
        {more > 0 && (
          <li className="text-fg-subtle">+{more} more</li>
        )}
      </ul>
    </div>
  );
}

// CardTags: compact chips from issue labels + content-derived tags.
// Display-only — filtering stays on the Inventory Tags dropdown.
function CardTags({ shown, more }: { shown: string[]; more: number }) {
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-1">
      {shown.map((tag) => (
        <Badge key={tag} variant="neutral" className="max-w-full truncate" title={tag}>
          {tag}
        </Badge>
      ))}
      {more > 0 && (
        <span className="text-micro text-fg-subtle" title={`${more} more tags`}>
          +{more}
        </span>
      )}
    </div>
  );
}

// CardTitle is plain text so a body click always opens the drawer (run console
// remains available from the footer run-id link).
function CardTitle({ card }: { card: PipelineBoardCardDTO }) {
  return (
    <div
      className="line-clamp-2 break-words text-sm font-medium leading-snug text-fg-default"
      title={card.title}
    >
      {card.title}
    </div>
  );
}

function StatusChip({ status }: { status?: string }) {
  if (!status) return null;
  return isKnownStatus(status) ? (
    <StatusBadge status={status} />
  ) : (
    <Badge>{status}</Badge>
  );
}

// --- per-lane STATUS (the card's only body content) -------------------------

// PriorityBadge mirrors /board's P{n} chip. On this board the number is not
// just a sort key: the admission loop launches ready tickets highest
// priority first (ties oldest-first), so P drives WHICH pipeline goes next.
function PriorityBadge({ priority }: { priority?: number }) {
  if (!priority || priority <= 0) return null;
  return (
    <Badge
      variant="warning"
      title={`Priority ${priority} — higher numbers launch first once ready`}
    >
      P{priority}
    </Badge>
  );
}

// Todo: every not-yet-running ticket. Ready badge = cleared to leave Todo for
// In progress (same predicate as the column "Ready" filter). Open hard deps
// get a separate blocked chip so they never look launchable.
function TodoStatus({ card }: { card: PipelineBoardCardDTO }) {
  const ready = cardReady(card);
  const queuePosition = card.queue_position ?? 0;
  const openBlockers = card.open_blocker_count ?? 0;
  const waitingDeps =
    card.issue_state === "waiting_deps" ||
    card.launch_blocked_reason === "waiting_deps" ||
    card.launch_blocked_reason === "open_blockers" ||
    card.launch_blocked_reason === "blocker_labels";
  return (
    <div className="flex flex-wrap items-center gap-1">
      <PriorityBadge priority={card.priority} />
      {ready ? (
        <Badge
          variant="success"
          title="Ready — will move to In progress when a concurrency slot frees"
        >
          Ready
        </Badge>
      ) : waitingDeps || openBlockers > 0 ? (
        <Badge
          variant="warning"
          title={
            openBlockers > 0
              ? `Blocked by ${openBlockers} open hard dep${openBlockers === 1 ? "" : "s"} — will not leave Todo until they are done`
              : "Waiting on dependencies — not ready for In progress yet"
          }
        >
          {openBlockers > 0 ? `Blocked by ${openBlockers}` : "Waiting on deps"}
        </Badge>
      ) : (
        <Badge
          variant="neutral"
          title="Not ready — Mark ready when the ticket can enter In progress"
        >
          Not ready
        </Badge>
      )}
      {queuePosition > 0 && (
        <Badge variant="warning" title={`Queue position ${queuePosition}`}>
          #{queuePosition}
        </Badge>
      )}
    </div>
  );
}

// In progress: tree progress + a Blocked tag naming WHY (the pending human
// gate) when the pipeline waits on a review. The review form itself lives
// exclusively in the details sidebar. While blocked, the root's own status
// chip is NOISE — Blocked already says everything the operator can act on.
// A resumable process state (e.g. a restart orphaned the parked parent) is
// the SYSTEM's business, not the card's: the details sidebar explains it
// next to the review form, and #205 tracks making it self-heal entirely.
function InProgressStatus({ card }: { card: PipelineBoardCardDTO }) {
  const reviews = card.pending_reviews ?? [];
  const blockedLabel =
    reviews.length === 1
      ? `Blocked — human review${reviews[0]?.node_id ? ` · ${reviews[0].node_id}` : ""}`
      : `Blocked — ${reviews.length} human reviews`;
  return (
    <div className="min-w-0 space-y-2">
      <ProgressBar executed={card.tree_executed_nodes} total={card.tree_total_nodes} />
      <div className="flex min-w-0 flex-wrap items-center gap-1">
        {reviews.length > 0 ? (
          <Badge
            variant="warning"
            className="max-w-full truncate"
            title={`${blockedLabel} — open the card to answer it`}
          >
            {blockedLabel}
          </Badge>
        ) : (
          <StatusChip status={card.status} />
        )}
      </div>
    </div>
  );
}

function ProgressBar({ executed, total }: { executed: number; total: number }) {
  const pct = total > 0 ? Math.min(100, Math.round((executed / total) * 100)) : 0;
  return (
    <div className="flex flex-col gap-1">
      <div
        className="h-1.5 w-full overflow-hidden rounded-full bg-surface-3"
        role="progressbar"
        aria-valuenow={executed}
        aria-valuemin={0}
        aria-valuemax={total}
      >
        <div
          className="h-full rounded-full bg-info transition-all"
          style={{ width: `${pct}%` }}
        />
      </div>
      <div className="text-micro tabular-nums text-fg-subtle">
        {executed} / {total} nodes
      </div>
    </div>
  );
}

// ChildrenProgressBar mirrors In-progress node bars: full-width track +
// "N / M closed" caption (campaign progress for planner parents).
function ChildrenProgressBar({
  closed,
  total,
  detail,
}: {
  closed: number;
  total: number;
  detail?: string;
}) {
  if (total <= 0) return null;
  const pct = Math.min(100, Math.round((closed / total) * 100));
  return (
    <div className="flex min-w-0 flex-col gap-1" title={detail || undefined}>
      <div
        className="h-1.5 w-full overflow-hidden rounded-full bg-surface-3"
        role="progressbar"
        aria-valuenow={closed}
        aria-valuemin={0}
        aria-valuemax={total}
        aria-label={`${closed} of ${total} children closed`}
      >
        <div
          className="h-full rounded-full bg-success transition-all"
          style={{ width: `${pct}%` }}
        />
      </div>
      <div className="text-micro tabular-nums text-fg-subtle">
        {closed} / {total} closed
      </div>
    </div>
  );
}

// Closed: every finished pipeline, success or failure. A Success badge for a
// clean finish (output in the sidebar's Result); for a failure, a Failed badge
// + the exact status chip (cancelled vs failed vs resumable) + the REASON, and
// the priority (a retried ticket re-enters the launch order with it). Retry /
// Edit live in the footer for ticket-backed failed cards.
//
// Long error strings (stack traces, LLM dumps) are clamped to 2 lines — full
// text is on title hover and in the details drawer.
function ClosedStatus({ card }: { card: PipelineBoardCardDTO }) {
  const failed = closedOutcome(card) === "failed";
  if (!failed) {
    return (
      <div className="flex flex-wrap items-center gap-1">
        <Badge variant="success" title="Finished successfully">
          Success
        </Badge>
      </div>
    );
  }
  return (
    <div className="min-w-0 space-y-1.5">
      <div className="flex flex-wrap items-center gap-1">
        <PriorityBadge priority={card.priority} />
        <Badge variant="danger">Failed</Badge>
        <StatusChip status={card.status} />
      </div>
      {card.error && (
        <div title={card.error} className="min-w-0">
          <InlineBanner tone="danger" layout="inline" className="min-w-0 py-1.5">
            <p className="line-clamp-2 break-words whitespace-pre-wrap">
              {card.error}
            </p>
          </InlineBanner>
        </div>
      )}
    </div>
  );
}
