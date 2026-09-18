import { useCallback, useEffect, useMemo, useRef, type ReactNode } from "react";
import { useLocation } from "wouter";
import {
  GroupedTableVirtuoso,
  GroupedVirtuoso,
  type ItemProps,
  type TableComponents,
} from "react-virtuoso";

import {
  BarChartIcon,
  ReloadIcon,
  RocketIcon,
} from "@radix-ui/react-icons";

import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { EmptyState } from "@/components/ui/EmptyState";
import type { RunRepo, RunSourceKind, RunStatus, RunSummary } from "@/api/runs";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useConfirm } from "@/hooks/useConfirm";
import { useRuns } from "@/hooks/useRuns";
import { useRunRepos } from "@/hooks/useRunRepos";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import QueueDepthBar from "./QueueDepthBar";
import {
  availableSourceKinds,
  filterRuns,
} from "./runListFilter";
import { metaForSource, runSourceKind } from "./runSourceMeta";
import { availableBots, type BotDescriptor } from "./runBotMeta";
import {
  availableRepos,
  type RepoChip,
  type RunMode,
} from "./runRepoMeta";
import {
  groupOptionsFor,
  groupRuns,
  sortRuns,
} from "./runListSortGroup";

import type { FilterMenuOption } from "./runList/FilterMenu";
import { RunListCard } from "./runList/RunListCard";
import {
  STATUS_FILTERS,
  statusFilterLabel,
} from "./runList/runListFormat";
import { RunListFilters } from "./runList/RunListFilters";
import { RunListRow } from "./runList/RunListRow";
import { RunListSkeleton } from "./runList/RunListSkeleton";
import { RunCardGroupHeader } from "./runList/RunCardGroupHeader";
import { runListBodyState, showRefreshingOverlay } from "./runList/runListBodyState";
import { RunSelectionToolbar } from "./runList/RunSelectionToolbar";
import { SortGroupControls } from "./runList/SortGroupControls";
import { useRunListActions } from "./runList/useRunListActions";
import { useRunListFilters } from "./runList/useRunListFilters";
import { useRunListLiveTick } from "./runList/useRunListLiveTick";
import { useRunListSelection } from "./runList/useRunListSelection";

// Shared per-row wiring handed to the virtualized table via Virtuoso's
// `context` prop. Keeping callbacks + selection state here (rather than
// closing over them in itemContent) lets the memoised TableRow read them
// without every render allocating a new row renderer.
interface RunTableContext {
  selectedIds: ReadonlySet<string>;
  resumingIds: ReadonlySet<string>;
  onOpen: (id: string) => void;
  onFilterBot: (botKey: string) => void;
  onToggleSelect: (id: string) => void;
  onResume: (id: string) => void;
}

// Custom <tr> for GroupedTableVirtuoso: carries the row click/hover the
// old static table had. Virtuoso supplies `item` (the run) + data-index
// (spread through `rest`) and owns positioning; we only add interaction
// + styling.
function RunTableRow({
  item,
  context,
  children,
  ...rest
}: ItemProps<RunSummary> & { context?: RunTableContext }) {
  return (
    <tr
      {...rest}
      className="group border-b border-border-default hover:bg-surface-2 cursor-pointer"
      onClick={() => context?.onOpen(item.id)}
    >
      {children}
    </tr>
  );
}

const RUN_TABLE_COMPONENTS: TableComponents<RunSummary, RunTableContext> = {
  Table: (props) => <table {...props} className="w-full text-xs" />,
  TableRow: RunTableRow,
};

export default function RunListView() {
  const [, setLocation] = useLocation();

  // Server mode selects the repo axis: cloud filters by repository
  // (project_path, server-side); local/desktop filters by folder
  // (repo_root||work_dir, client-side). Anything non-cloud is "local".
  const serverMode = useServerInfoStore((s) => s.info?.mode);
  const mode: RunMode = serverMode === "cloud" ? "cloud" : "local";

  const {
    status,
    setStatus,
    queryInput,
    setQueryInput,
    query,
    since,
    setSince,
    source,
    setSource,
    bot,
    setBot,
    repo,
    setRepo,
    sort,
    setSort,
    group,
    setGroup,
  } = useRunListFilters();

  // The repo axis splits by mode: cloud filters server-side (index-backed,
  // a project_path slug), local filters client-side (a folder path that is
  // not a project_path and would match nothing server-side). Resolve the
  // split once so neither the fetch nor filterRuns has to know about mode.
  const serverRepo = mode === "cloud" ? repo : "";
  const clientRepo = mode === "cloud" ? "" : repo;

  // Repo-first scope: the sidebar's active repo re-anchors this list
  // whenever it CHANGES; the chips stay usable as a local override until
  // the next scope change. A ?repo= deep-link wins over the initial
  // scope adoption so shared URLs keep their filter.
  const { activeRepo, overview, enabled: repoScope, loading: repoScopeLoading } = useActiveRepo();
  const scopeRepoName = repoScope && !overview ? (activeRepo?.repo_full_name ?? "") : "";
  const seenScopeRef = useRef<string | null>(null);
  useEffect(() => {
    if (mode !== "cloud" || !repoScope || repoScopeLoading) return;
    const prev = seenScopeRef.current;
    seenScopeRef.current = scopeRepoName;
    if (prev === null) {
      if (repo === "" && scopeRepoName !== "") setRepo(scopeRepoName);
      return;
    }
    if (prev !== scopeRepoName) setRepo(scopeRepoName);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, repoScope, repoScopeLoading, scopeRepoName]);

  const { runs, counts, loading, refreshing, error } = useRuns({
    status,
    repo: serverRepo,
  });

  const filteredRuns = useMemo(
    () => filterRuns(runs, { query, since, source, bot, repo: clientRepo }),
    [runs, query, since, source, bot, clientRepo],
  );

  // Source-filter chip strip: only show kinds present in the current
  // (status-filtered) fetched list. Recomputed off `runs` (not the
  // post-filter list) so picking "Webhook" doesn't make the other
  // chips vanish.
  const availableSources = useMemo(() => availableSourceKinds(runs), [runs]);

  // Per-source counts for the chip strip — informational, mirrors the
  // status chip's count rendering.
  const sourceCounts = useMemo(() => {
    const m: Partial<Record<RunSourceKind, number>> = {};
    for (const r of runs) {
      const k = runSourceKind(r);
      m[k] = (m[k] ?? 0) + 1;
    }
    return m;
  }, [runs]);

  // Bot-filter chip strip: distinct bots (with counts) present in the
  // fetched list — computed off `runs` (not the post-filter list) so
  // picking one bot doesn't hide the others. An active bot with no
  // surviving runs is prepended so it stays visible and clearable.
  const botChips = useMemo<BotDescriptor[]>(() => {
    const base = availableBots(runs);
    if (bot === "" || base.some((b) => b.key === bot)) return base;
    return [{ key: bot, label: bot, emoji: "🤖", count: 0 }, ...base];
  }, [runs, bot]);

  // Repo/folder chip strip. Cloud: the index-backed distinct-repos
  // endpoint, decoupled from the list so selecting a repo (which narrows
  // the server-side fetch) doesn't empty the strip. Local: derived
  // client-side from the fetched runs' folders. An active value absent
  // from the set is prepended so it stays clearable.
  const { repos: cloudRepos } = useRunRepos(mode === "cloud");
  const repoChips = useMemo<RepoChip[]>(() => {
    const base: RepoChip[] =
      mode === "cloud"
        ? cloudRepos.map((r: RunRepo) => ({
            key: r.project_path,
            label: r.project_path,
            title: r.project_path,
            count: r.count,
          }))
        : availableRepos(runs);
    if (repo === "" || base.some((c) => c.key === repo)) return base;
    return [{ key: repo, label: repo, title: repo, count: 0 }, ...base];
  }, [mode, cloudRepos, runs, repo]);

  const openRun = useCallback(
    (id: string) => setLocation(`/runs/${encodeURIComponent(id)}`),
    [setLocation],
  );

  // Click a row's bot avatar to filter to that bot — or toggle it off
  // when it's already the active bot. Stable so memoised rows don't
  // re-render when other state changes.
  const filterByBot = useCallback(
    (key: string) => setBot((prev) => (prev === key ? "" : key)),
    [setBot],
  );

  const { now } = useRunListLiveTick(filteredRuns);

  // Sort + group are layered on top of the filtered list. We anchor
  // the "duration" sort and the in-flight tick on a captured `now` so
  // re-renders within the same tick produce a stable order.
  const sortedRuns = useMemo(
    () => sortRuns(filteredRuns, sort, now),
    [filteredRuns, sort, now],
  );
  const groups = useMemo(() => groupRuns(sortedRuns, group), [sortedRuns, group]);
  const isGrouped = group !== "none";

  // Virtualization model. GroupedVirtuoso / GroupedTableVirtuoso want a
  // flat item list plus per-group counts; the group index is implicit
  // from the counts. We flatten once per (groups) change so scrolling
  // and the live-duration tick don't rebuild it.
  const flatRuns = useMemo(
    () => groups.flatMap((g) => g.runs),
    [groups],
  );
  const groupCounts = useMemo(
    () => groups.map((g) => g.runs.length),
    [groups],
  );
  const groupLabels = useMemo(
    () => groups.map((g) => ({ label: g.label, count: g.runs.length })),
    [groups],
  );

  // Multi-selection over the visible (filtered + sorted) list, plus the
  // mutations it drives (inline Resume, bulk cancel/delete). Selection
  // checkboxes exist on the desktop table only.
  const { selectedIds, selectedRuns, allSelected, toggle, toggleAll, clear } =
    useRunListSelection(sortedRuns);
  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog } = useConfirm();
  const { onResume, resumingIds, onBulkCancel, onBulkDelete } =
    useRunListActions({
      selectedRuns,
      clearSelection: clear,
      addToast,
      confirm,
      onOpenRun: openRun,
    });
  const someSelected = selectedIds.size > 0;

  // Context handed to the virtualized table's row component. Rebuilt only
  // when the selection/resume sets or the flat list change — the callbacks
  // are already stable via useCallback.
  const runTableContext = useMemo<RunTableContext>(
    () => ({
      selectedIds,
      resumingIds,
      onOpen: openRun,
      onFilterBot: filterByBot,
      onToggleSelect: toggle,
      onResume,
    }),
    [selectedIds, resumingIds, openRun, filterByBot, toggle, onResume],
  );

  // Shared "All" option count for the source + bot menus (the full
  // status-filtered fetch size). Omitted when zero.
  const totalCount = runs.length > 0 ? runs.length : undefined;

  const statusOptions = useMemo<FilterMenuOption[]>(
    () =>
      STATUS_FILTERS.map((f) => {
        const n =
          f.value === "" ? runs.length : counts[f.value as RunStatus] ?? 0;
        return { key: f.value, label: f.label, count: n > 0 ? n : undefined };
      }),
    [runs.length, counts],
  );

  // Per-axis menu option lists. Status/Since are tiny and built inline in
  // the JSX; the data-driven axes are memoised because they map over
  // potentially many bots/repos and build label nodes.
  const sourceOptions = useMemo<FilterMenuOption[]>(
    () => [
      { key: "", label: "All", count: totalCount },
      ...availableSources.map((kind) => {
        const meta = metaForSource(kind);
        const Icon = meta.Icon;
        return {
          key: kind,
          label: (
            <span className="inline-flex items-center gap-1.5" title={meta.description}>
              <Icon className="w-3 h-3" />
              <span>{meta.label}</span>
            </span>
          ),
          count: sourceCounts[kind],
        };
      }),
    ],
    [availableSources, sourceCounts, totalCount],
  );

  const botOptions = useMemo<FilterMenuOption[]>(
    () => [
      { key: "", label: "All", count: totalCount },
      ...botChips.map((b) => ({
        key: b.key,
        label: (
          <span className="inline-flex items-center gap-1.5">
            <span>{b.emoji}</span>
            <span>{b.label}</span>
          </span>
        ),
        count: b.count > 0 ? b.count : undefined,
      })),
    ],
    [botChips, totalCount],
  );

  const repoOptions = useMemo<FilterMenuOption[]>(
    () => [
      { key: "", label: "All" },
      ...repoChips.map((c) => ({
        key: c.key,
        label: c.label,
        count: c.count > 0 ? c.count : undefined,
      })),
    ],
    [repoChips],
  );

  const filtersActive =
    query !== "" ||
    since !== "all" ||
    source !== "" ||
    bot !== "" ||
    repo !== "";
  const clearFilters = useCallback(() => {
    setQueryInput("");
    setSince("all");
    setSource("");
    setBot("");
    setRepo("");
  }, [setQueryInput, setSince, setSource, setBot, setRepo]);

  const bodyState = runListBodyState({
    loading,
    error,
    runCount: runs.length,
    filteredCount: filteredRuns.length,
  });

  let body: ReactNode;
  if (bodyState === "skeleton") {
    // Cold load — no cached data for this scope yet. Show the skeleton
    // (matches the table/card layout so nothing jumps when rows land). Its
    // own scroll wrapper since the container is overflow-hidden for the
    // virtualized list.
    body = (
      <div className="h-full overflow-auto">
        <RunListSkeleton />
      </div>
    );
  } else if (bodyState === "error") {
    body = <EmptyState message={<span className="text-danger">{error}</span>} />;
  } else if (bodyState === "empty") {
    // Cloud operators launch from /bots or the pipeline board, not the
    // editor. Desktop/local operators do open the editor as the primary
    // launch surface, so keep those CTAs there.
    body =
      mode === "cloud" ? (
        <EmptyState
          title="No runs yet"
          message="Pick a bot from the gallery to launch your first run."
          caret
          action={
            <Button
              variant="primary"
              size="sm"
              leadingIcon={<RocketIcon />}
              onClick={() => setLocation("/bots")}
            >
              Browse bots
            </Button>
          }
          secondaryAction={
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setLocation("/")}
            >
              Home
            </Button>
          }
        />
      ) : (
        <EmptyState
          title="No runs yet"
          message="Launch a workflow from the editor to populate this list."
          caret
          action={
            <Button
              variant="primary"
              size="sm"
              leadingIcon={<RocketIcon />}
              onClick={() => setLocation("/editor")}
            >
              Open editor
            </Button>
          }
          secondaryAction={
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setLocation("/")}
            >
              Home
            </Button>
          }
        />
      );
  } else if (bodyState === "no-matches") {
    body =
      status !== "" ? (
        <EmptyState
          title={`No ${statusFilterLabel(status)} runs`}
          message="Pick a different status, or clear the filters to see everything."
          action={
            <Button variant="secondary" size="sm" onClick={() => setStatus("")}>
              Show all statuses
            </Button>
          }
          secondaryAction={
            filtersActive ? (
              <Button variant="ghost" size="sm" onClick={clearFilters}>
                Clear search / date
              </Button>
            ) : undefined
          }
        />
      ) : (
        <EmptyState
          title="No matching runs"
          message="Try a different search term or widen the date range."
          action={
            <Button variant="secondary" size="sm" onClick={clearFilters}>
              Clear filters
            </Button>
          }
        />
      );
  } else {
    body = (
      <>
        {/* Desktop / tablet: virtualized semantic table. Only the visible
            rows mount, so a several-hundred-run list no longer blocks the
            main thread on a scope switch. thead (with select-all), group
            headers, columns and selection all survive virtualization. The
            Source column tells the operator how each run was triggered. */}
        <div className="hidden sm:block h-full">
          <GroupedTableVirtuoso<RunSummary, RunTableContext>
            className="h-full"
            data={flatRuns}
            groupCounts={groupCounts}
            context={runTableContext}
            components={RUN_TABLE_COMPONENTS}
            fixedHeaderContent={() => (
              <tr className="border-b border-border-default bg-surface-1 text-fg-subtle">
                <th scope="col" className="pl-4 pr-1 py-2 w-8">
                  <Checkbox
                    aria-label="Select all runs"
                    checked={allSelected}
                    ref={(el) => {
                      if (el) el.indeterminate = someSelected && !allSelected;
                    }}
                    onChange={toggleAll}
                  />
                </th>
                <th scope="col" className="text-left px-4 py-2 font-medium">Run</th>
                <th scope="col" className="text-left px-4 py-2 font-medium">Workflow</th>
                <th scope="col" className="text-left px-4 py-2 font-medium">Source</th>
                <th scope="col" className="text-left px-4 py-2 font-medium">Status</th>
                <th scope="col" className="text-left px-4 py-2 font-medium">Started</th>
                <th scope="col" className="text-left px-4 py-2 font-medium">Duration</th>
                <th scope="col" className="text-left px-4 py-2 font-medium">Run ID</th>
              </tr>
            )}
            groupContent={(index) =>
              isGrouped ? (
                <th
                  colSpan={8}
                  scope="rowgroup"
                  className="text-left px-4 py-1.5 font-medium text-fg-muted text-micro uppercase tracking-wide bg-surface-2 border-y border-border-default"
                >
                  <span>{groupLabels[index]?.label}</span>
                  <span className="ml-2 text-fg-subtle normal-case tracking-normal">
                    {groupLabels[index]?.count}
                  </span>
                </th>
              ) : (
                // No grouping: Virtuoso still requires a group row. Render
                // an empty, zero-height header so nothing shows.
                <th colSpan={8} className="p-0 h-0" aria-hidden />
              )
            }
            itemContent={(_index, _groupIndex, run, ctx) => (
              <RunListRow
                run={run}
                selected={ctx.selectedIds.has(run.id)}
                resuming={ctx.resumingIds.has(run.id)}
                onFilterBot={ctx.onFilterBot}
                onToggleSelect={ctx.onToggleSelect}
                onResume={ctx.onResume}
              />
            )}
            computeItemKey={(index) => flatRuns[index]?.id ?? index}
          />
        </div>

        {/* Mobile: virtualized card list. Group headers render inline via
            groupContent; divide-y is preserved by the row borders. */}
        <div className="sm:hidden h-full">
          <GroupedVirtuoso<RunSummary, RunTableContext>
            className="h-full"
            data={flatRuns}
            groupCounts={groupCounts}
            context={runTableContext}
            groupContent={(index) =>
              isGrouped ? (
                <RunCardGroupHeader
                  label={groupLabels[index]?.label ?? ""}
                  count={groupLabels[index]?.count ?? 0}
                />
              ) : (
                <div className="h-0" aria-hidden />
              )
            }
            itemContent={(_index, _groupIndex, run, ctx) =>
              run ? (
                <div className="border-b border-border-default">
                  <RunListCard
                    run={run}
                    resuming={ctx.resumingIds.has(run.id)}
                    onOpen={ctx.onOpen}
                    onFilterBot={ctx.onFilterBot}
                    onResume={ctx.onResume}
                  />
                </div>
              ) : null
            }
            computeItemKey={(index) => flatRuns[index]?.id ?? index}
          />
        </div>
      </>
    );
  }

  // A scope switch (team/org/repo) is in flight when keepPreviousData is
  // holding the previous scope's rows (refreshing) or the repo scope is
  // still resolving. During that window every run-derived surface — the
  // list AND the queue-depth bar / filter chip counts — is showing the
  // previous scope's numbers, so we dim them together to signal "these are
  // being replaced" instead of dimming only the list under the overlay.
  const scopeSwitching = showRefreshingOverlay({
    refreshing,
    repoScopeLoading,
    runCount: runs.length,
  });

  return (
    <div className="h-full flex flex-col overflow-hidden bg-surface-1 text-fg-default">
      <div
        className={
          scopeSwitching ? "opacity-60 transition-opacity" : "transition-opacity"
        }
      >
        <QueueDepthBar counts={counts} />
      </div>

      <div className="px-4 py-2 flex flex-wrap items-center gap-2 border-b border-border-default">
        <RunListFilters
          queryInput={queryInput}
          onQueryChange={setQueryInput}
          status={status}
          statusOptions={statusOptions}
          onStatus={setStatus}
          source={source}
          sourceOptions={sourceOptions}
          showSourceFilter={availableSources.length > 0}
          onSource={setSource}
          bot={bot}
          botOptions={botOptions}
          showBotFilter={botChips.length > 1 || bot !== ""}
          onBot={setBot}
          repo={repo}
          repoOptions={repoOptions}
          showRepoFilter={repoChips.length > 1 || repo !== ""}
          mode={mode}
          onRepo={setRepo}
          since={since}
          onSince={setSince}
          filtersActive={filtersActive}
          onClearFilters={clearFilters}
        />

        <div className="ml-auto flex items-center gap-2">
          <SortGroupControls
            sort={sort}
            onSort={setSort}
            group={group}
            onGroup={setGroup}
            groupOptions={groupOptionsFor(mode)}
          />
          <Button
            variant="ghost"
            size="sm"
            leadingIcon={<BarChartIcon />}
            onClick={() => setLocation("/insights")}
            title="Cross-run cost, fail rate, and duration over a configurable window"
          >
            Analytics
          </Button>
        </div>
      </div>

      {someSelected && (
        <RunSelectionToolbar
          selectedRuns={selectedRuns}
          onCancel={() => void onBulkCancel()}
          onDelete={() => void onBulkDelete()}
          onClear={clear}
        />
      )}

      {/* overflow-hidden, not overflow-auto: the virtualized list owns its
          own scroller, so a scroll here would nest a second scrollbar. The
          skeleton / empty / error bodies fit the viewport or scroll
          internally. */}
      <div className="relative flex-1 min-h-0 overflow-hidden">
        <div className={`h-full ${scopeSwitching ? "opacity-60 transition-opacity" : "transition-opacity"}`}>
          {body}
        </div>
        {/* Scope-switch indicator. keepPreviousData keeps the previous
            scope's list on screen; this dims it (above) and shows a small
            pill so the reload is visible without blanking. Shown only on a
            real scope switch (isPlaceholderData) — never on a same-key poll
            tick. pointer-events-none so it never blocks scroll / selection. */}
        {scopeSwitching && (
          <div
            className="pointer-events-none absolute inset-0 flex items-start justify-center"
            role="status"
            aria-live="polite"
            aria-label="Refreshing runs"
          >
            <span className="mt-3 inline-flex items-center gap-1.5 rounded-full border border-border-default bg-surface-1 px-2.5 py-1 text-caption text-fg-muted shadow-sm">
              <ReloadIcon className="h-3 w-3 animate-spin" />
              Updating…
            </span>
          </div>
        )}
      </div>
      {dialog}
    </div>
  );
}

// Re-exported for the runListStatusLabel unit test.
export { statusFilterLabel } from "./runList/runListFormat";
