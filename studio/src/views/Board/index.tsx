import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useSearch } from "wouter";

import { formatRelative } from "@/lib/format";

import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import DispatcherControlBar from "@/components/shared/DispatcherControlBar";
import { Button } from "@/components/ui/Button";
import {
  cancelIssue,
  getState,
  type RetryView,
  type RunningView,
} from "@/api/dispatcher";
import {
  createIssue,
  deleteIssue,
  getBoard,
  listIssues,
  patchIssue,
  transitionIssue,
  type NativeBoard,
  type NativeIssue,
} from "@/api/native";
import IssueModal from "./IssueModal";
import SettingsDrawer from "@/components/Dispatcher/SettingsDrawer";
import TrackerErrorBanner from "@/components/shared/TrackerErrorBanner";
import { useBoardKeyboard } from "@/hooks/useBoardKeyboard";

export default function BoardView() {
  const [, setLocation] = useLocation();
  const search = useSearch();
  const focusFromUrl = useMemo(() => {
    return new URLSearchParams(search).get("focus");
  }, [search]);
  const [board, setBoard] = useState<NativeBoard | null>(null);
  const [issues, setIssues] = useState<NativeIssue[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [editing, setEditing] = useState<NativeIssue | null>(null);
  const [creating, setCreating] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [runningByIssue, setRunningByIssue] = useState<Map<string, RunningView>>(new Map());
  const [retryingByIssue, setRetryingByIssue] = useState<Map<string, RetryView>>(new Map());
  const [trackerError, setTrackerError] = useState<{ tracker: string; message: string } | null>(
    null,
  );
  const [dispatcherPaused, setDispatcherPaused] = useState(false);
  // History of recent column transitions, so Ctrl+Z reverts the last
  // one. Bounded at 10 entries — the board's drag-undo intent is the
  // immediate "oops, wrong column", not full session replay.
  const transitionHistoryRef = useRef<Array<{ id: string; from: string }>>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [helpOpen, setHelpOpen] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");
  const [labelFilter, setLabelFilter] = useState<Set<string>>(() => new Set());
  const [assigneeFilter, setAssigneeFilter] = useState("");
  // Single source of truth for label filter toggling — used both by
  // the top filter strip and by clicking a chip on any card. Lifted
  // here so card-level chips toggle the same Set the filter strip
  // shows.
  const onLabelToggle = useCallback((l: string) => {
    setLabelFilter((prev) => {
      const next = new Set(prev);
      if (next.has(l)) next.delete(l);
      else next.add(l);
      return next;
    });
  }, []);

  // Poll the dispatcher snapshot every 2s so each card can show a
  // running/retrying badge + cancel button. We ignore failures: when
  // the dispatcher is idle the snapshot is still returned (empty
  // running/retries), and a 5xx is rare enough that flashing the maps
  // empty would be more disruptive than keeping stale data.
  useEffect(() => {
    let alive = true;
    let inflight = false;
    let gen = 0;
    const tick = async () => {
      if (!alive || inflight) return;
      inflight = true;
      const myGen = ++gen;
      try {
        const snap = await getState();
        // Drop responses that arrive after a newer request has
        // started — without the gen guard, a slow getState resolving
        // after a fresh tick would clobber the newer state.
        if (!alive || myGen !== gen) return;
        const rmap = new Map<string, RunningView>();
        for (const r of snap.running ?? []) rmap.set(r.issue_id, r);
        const xmap = new Map<string, RetryView>();
        for (const r of snap.retries ?? []) xmap.set(r.issue_id, r);
        setRunningByIssue(rmap);
        setRetryingByIssue(xmap);
        // Guard the tracker error update on value equality so a stable
        // poll doesn't churn an identical object reference each tick
        // and re-render the whole board.
        setTrackerError((prev) => {
          const message = snap.last_tracker_error ?? "";
          if (!message) return prev === null ? prev : null;
          if (
            prev &&
            prev.tracker === snap.tracker &&
            prev.message === message
          ) {
            return prev;
          }
          return { tracker: snap.tracker, message };
        });
        setDispatcherPaused(!!snap.paused);
      } catch {
        // swallow: dispatcher may be unreachable / not wired
      } finally {
        inflight = false;
      }
    };
    void tick();
    const id = setInterval(() => void tick(), 2000);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, []);

  const onCancelRun = useCallback(async (issueID: string) => {
    try {
      await cancelIssue(issueID);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  const refresh = useCallback(async () => {
    setError(null);
    try {
      const [b, i] = await Promise.all([getBoard(), listIssues()]);
      setBoard(b);
      setIssues(i ?? []);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Apply the ?focus=<issueID> deep-link from the Dispatcher view's
  // retry-queue rows. Runs once after issues load so the auto-selected
  // card is actually present in state. Self-clears the param so a hard
  // reload doesn't re-focus on an issue the user has since moved on
  // from.
  useEffect(() => {
    if (!focusFromUrl) return;
    if (issues.length === 0) return;
    const match = issues.find((i) => i.id === focusFromUrl);
    if (!match) return;
    setSelectedId(match.id);
    setLocation("/board", { replace: true });
  }, [focusFromUrl, issues, setLocation]);

  // Distinct values exposed in the filter dropdowns. Derived from the
  // current issues list so the dropdowns track what the user actually
  // sees — including labels created on the fly by bots.
  const { allLabels, allAssignees } = useMemo(() => {
    const labels = new Set<string>();
    const assignees = new Set<string>();
    for (const iss of issues) {
      for (const l of iss.labels ?? []) labels.add(l);
      if (iss.assignee) assignees.add(iss.assignee);
    }
    return {
      allLabels: Array.from(labels).sort(),
      allAssignees: Array.from(assignees).sort(),
    };
  }, [issues]);

  // filteredIssues applies the search query + active label/assignee
  // filters to the raw issues list. Keep filtering client-side: the
  // backend's listIssues filter would force a full network round-trip
  // on every keystroke, which makes the search feel laggy on
  // multi-hundred-issue boards. Title/body substring is case-insensitive.
  const filteredIssues = useMemo(() => {
    const q = searchQuery.trim().toLowerCase();
    const labels = labelFilter;
    const assignee = assigneeFilter.trim();
    if (!q && labels.size === 0 && !assignee) return issues;
    return issues.filter((iss) => {
      if (q) {
        const hay =
          (iss.title ?? "") + "\t" + (iss.body ?? "") + "\t" + iss.id;
        if (!hay.toLowerCase().includes(q)) return false;
      }
      if (labels.size > 0) {
        const have = new Set(iss.labels ?? []);
        for (const l of labels) {
          if (!have.has(l)) return false;
        }
      }
      if (assignee && iss.assignee !== assignee) return false;
      return true;
    });
  }, [issues, searchQuery, labelFilter, assigneeFilter]);

  // Group filtered issues by state for column rendering. Issues whose
  // state does not appear on the board land in an "unmapped" bucket so
  // they are not silently lost when the operator renames a state.
  const byState = useMemo(() => {
    const m = new Map<string, NativeIssue[]>();
    if (!board) return m;
    for (const s of board.states) m.set(s.name, []);
    m.set("__unmapped__", []);
    for (const iss of filteredIssues) {
      const bucket = m.has(iss.state) ? iss.state : "__unmapped__";
      m.get(bucket)!.push(iss);
    }
    for (const list of m.values()) {
      list.sort((a, b) => (b.priority ?? 0) - (a.priority ?? 0));
    }
    return m;
  }, [board, filteredIssues]);

  // Eligible counts surfaced in the browser tab title so operators
  // with the board pinned in a background tab see new ready/
  // in-progress work without focusing it. Derived as a stable string
  // first so the effect only runs when the rendered counts actually
  // change — `byState` gets a fresh Map identity every render, which
  // would otherwise rewrite document.title every 2s on every poll
  // tick.
  const tabBadge = useMemo(() => {
    if (!board) return null;
    const eligible = board.states.filter((s) => s.eligible);
    if (eligible.length === 0) return null;
    const parts: string[] = [];
    for (const s of eligible) {
      const count = (byState.get(s.name) ?? []).length;
      if (count > 0) parts.push(`${count} ${s.display ?? s.name}`);
    }
    return parts.length > 0 ? `(${parts.join(", ")})` : null;
  }, [board, byState]);

  useEffect(() => {
    if (!tabBadge) return;
    const prev = document.title;
    document.title = `${tabBadge} ${prev}`;
    return () => {
      document.title = prev;
    };
  }, [tabBadge]);

  const recordTransition = useCallback((id: string, from: string) => {
    const hist = transitionHistoryRef.current;
    hist.push({ id, from });
    if (hist.length > 10) hist.shift();
  }, []);

  const onDrop = useCallback(
    async (issueID: string, toState: string, opts?: { recordHistory?: boolean }) => {
      const recordHistory = opts?.recordHistory ?? true;
      // Capture this invocation's pre-state in a per-call closure so
      // two near-simultaneous drops don't race over the same `before`
      // variable. The prior implementation hoisted `before` to the
      // outer scope and the second drop would overwrite the first
      // drop's snapshot before its async transitionIssue had a chance
      // to fail / roll back, restoring the wrong row.
      const draft: { snapshot: NativeIssue[]; prevState: string | null } = {
        snapshot: [],
        prevState: null,
      };
      setIssues((cur) => {
        draft.snapshot = cur;
        const found = cur.find((i) => i.id === issueID);
        draft.prevState = found?.state ?? null;
        return cur.map((i) => (i.id === issueID ? { ...i, state: toState } : i));
      });
      try {
        await transitionIssue(issueID, toState);
        if (recordHistory && draft.prevState && draft.prevState !== toState) {
          recordTransition(issueID, draft.prevState);
        }
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        // Only revert this issue's row to its pre-drop state — leave
        // other concurrent edits in place. Falls back to full revert
        // when the row was reordered out of the snapshot.
        const previous = draft.snapshot;
        setIssues((cur) => {
          const prev = previous.find((i) => i.id === issueID);
          if (!prev) return previous;
          return cur.map((i) => (i.id === issueID ? prev : i));
        });
      }
    },
    [recordTransition],
  );

  const undoLastTransition = useCallback(() => {
    const last = transitionHistoryRef.current.pop();
    if (!last) return;
    // Avoid re-recording the undo as a new history entry — otherwise
    // the user would just toggle between two columns forever.
    void onDrop(last.id, last.from, { recordHistory: false });
  }, [onDrop]);

  // Wire Ctrl+Z to the undo-last-transition. Skip when a modal owns
  // focus or an input is active so we don't fight form fields.
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (!(e.ctrlKey || e.metaKey) || e.key !== "z" || e.shiftKey) return;
      const target = e.target as HTMLElement | null;
      const inInput =
        target?.tagName === "INPUT" ||
        target?.tagName === "TEXTAREA" ||
        target?.isContentEditable;
      if (inInput) return;
      if (creating || editing || helpOpen || settingsOpen) return;
      if (transitionHistoryRef.current.length === 0) return;
      e.preventDefault();
      undoLastTransition();
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [creating, editing, helpOpen, settingsOpen, undoLastTransition]);

  const onCreate = useCallback(
    async (input: Partial<NativeIssue>) => {
      try {
        await createIssue({
          title: input.title ?? "",
          body: input.body,
          state: input.state,
          labels: input.labels,
          priority: input.priority,
          assignee: input.assignee,
          fields: input.fields,
          bot: input.bot,
          bot_args: input.bot_args,
        });
        setCreating(false);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [refresh],
  );

  const onSave = useCallback(
    async (input: Partial<NativeIssue>) => {
      if (!editing) return;
      try {
        await patchIssue(editing.id, {
          title: input.title,
          body: input.body,
          labels: input.labels,
          priority: input.priority,
          assignee: input.assignee,
          fields: input.fields,
          // bot defaults to "" when cleared — send the empty string so
          // the Patch.Bot pointer is set, allowing the server to clear
          // a previously-set bot. Same for bot_args (empty map clears).
          bot: input.bot ?? "",
          bot_args: input.bot_args ?? {},
        });
        setEditing(null);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [editing, refresh],
  );

  const onDelete = useCallback(
    async (id: string) => {
      if (
        !confirm(
          "Delete this issue? This removes it from the board and cannot be undone.",
        )
      )
        return;
      try {
        await deleteIssue(id);
        setEditing(null);
        setSelectedId((cur) => (cur === id ? null : cur));
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [refresh],
  );

  useBoardKeyboard({
    board,
    byState,
    selectedId,
    modalOpen: creating || editing !== null || helpOpen || settingsOpen,
    onSelect: setSelectedId,
    onCreate: () => setCreating(true),
    onEdit: (id) => {
      const iss = issues.find((i) => i.id === id);
      if (iss) setEditing(iss);
    },
    onDelete: (id) => void onDelete(id),
    onTransition: (id, toState) => void onDrop(id, toState),
    onShowHelp: () => setHelpOpen((v) => !v),
  });

  useHeaderSlot({
    left: <span className="text-xs font-medium text-fg-default">Board</span>,
    right: board ? (
      <>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => setLocation("/board/labels")}
          title="Manage the board's label vocabulary"
        >
          Labels
        </Button>
        <Button variant="secondary" size="sm" onClick={() => void refresh()}>
          Refresh
        </Button>
        <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
          + New issue
        </Button>
      </>
    ) : null,
  });

  if (loading) {
    return <div className="p-8 text-fg-muted">Loading kanban…</div>;
  }
  if (!board) {
    return <EmptyBoard kind="missing" />;
  }

  return (
    <div className="h-full flex flex-col overflow-hidden">
      <DispatcherControlBar onOpenSettings={() => setSettingsOpen(true)} />
      <SettingsDrawer
        open={settingsOpen}
        onClose={() => setSettingsOpen(false)}
        onSaved={() => void refresh()}
      />

      {error && (
        <div className="bg-red-500/10 border-b border-red-500/40 px-4 py-2 text-xs text-red-200">
          {error}
        </div>
      )}
      {trackerError && (
        <TrackerErrorBanner
          tracker={trackerError.tracker}
          message={trackerError.message}
        />
      )}
      {dispatcherPaused && (
        <div className="bg-yellow-500/10 border-b border-yellow-500/40 px-4 py-2 text-xs text-yellow-200 flex items-center gap-2">
          <span className="font-medium">Dispatcher paused</span>
          <span className="text-yellow-200/80">
            New issues won't be dispatched until you resume from the toolbar
            above. In-flight runs continue unaffected.
          </span>
        </div>
      )}

      <BoardFilters
        searchQuery={searchQuery}
        labelFilter={labelFilter}
        assigneeFilter={assigneeFilter}
        allLabels={allLabels}
        allAssignees={allAssignees}
        total={issues.length}
        filtered={filteredIssues.length}
        onSearchChange={setSearchQuery}
        onLabelToggle={onLabelToggle}
        onAssigneeChange={setAssigneeFilter}
        onReset={() => {
          setSearchQuery("");
          setLabelFilter(new Set());
          setAssigneeFilter("");
        }}
      />

      {issues.length === 0 && (
        <EmptyBoardBanner onCreate={() => setCreating(true)} />
      )}
      <div className="flex-1 overflow-auto p-3">
        <div className="flex gap-3 min-w-fit">
          {board.states.map((s) => (
            <Column
              key={s.name}
              name={s.name}
              display={s.display ?? s.name}
              terminal={!!s.terminal}
              eligible={!!s.eligible}
              color={s.color ?? defaultStateColor(s.name, !!s.eligible, !!s.terminal)}
              issues={byState.get(s.name) ?? []}
              selectedId={selectedId}
              runningByIssue={runningByIssue}
              retryingByIssue={retryingByIssue}
              onDrop={onDrop}
              onSelectCard={setSelectedId}
              onClickCard={(iss) => setEditing(iss)}
              onLabelClick={onLabelToggle}
              activeLabels={labelFilter}
              onCancelRun={onCancelRun}
              onOpenRun={(runId) => setLocation(`/runs/${encodeURIComponent(runId)}`)}
              dimmed={dispatcherPaused}
            />
          ))}
          {(byState.get("__unmapped__")?.length ?? 0) > 0 && (
            <Column
              name="__unmapped__"
              display="Unmapped"
              terminal={false}
              eligible={false}
              color="var(--color-board-backlog)"
              issues={byState.get("__unmapped__") ?? []}
              selectedId={selectedId}
              runningByIssue={runningByIssue}
              retryingByIssue={retryingByIssue}
              onDrop={onDrop}
              onSelectCard={setSelectedId}
              onClickCard={(iss) => setEditing(iss)}
              onLabelClick={onLabelToggle}
              activeLabels={labelFilter}
              onCancelRun={onCancelRun}
              onOpenRun={(runId) => setLocation(`/runs/${encodeURIComponent(runId)}`)}
              dimmed={dispatcherPaused}
            />
          )}
        </div>
      </div>

      {creating && (
        <IssueModal
          board={board}
          initial={null}
          onSubmit={onCreate}
          onClose={() => setCreating(false)}
        />
      )}
      {editing && (
        <IssueModal
          board={board}
          initial={editing}
          onSubmit={onSave}
          onClose={() => setEditing(null)}
          onDelete={() => void onDelete(editing.id)}
        />
      )}
      {helpOpen && <BoardKeyboardHelp onClose={() => setHelpOpen(false)} />}
    </div>
  );
}

// EmptyBoard renders the "tracker not initialised on disk yet" guide.
// The "board exists but has no issues" case is handled inline above the
// columns by EmptyBoardBanner so the column headers stay visible.
function EmptyBoard({ kind }: { kind: "missing" }) {
  if (kind === "missing") {
    return (
      <div className="p-8 max-w-lg mx-auto text-fg-default space-y-4">
        <div className="text-lg font-semibold">Native tracker not initialised</div>
        <p className="text-sm text-fg-muted">
          The board view persists issues under the project's{" "}
          <code className="text-xs bg-surface-2 px-1 rounded">.iterion/dispatcher/native/</code>{" "}
          directory. iterion creates one automatically on first launch.
        </p>
        <div className="text-sm">
          <p className="mb-1 text-fg-default">Start it from the workspace:</p>
          <pre className="bg-surface-2 rounded p-2 text-xs font-mono overflow-x-auto">
            iterion studio --dir &lt;your-project&gt;
          </pre>
        </div>
      </div>
    );
  }
  return null;
}

const EMPTY_BANNER_DISMISSED_KEY = "iterion.board.empty-banner-dismissed";

// EmptyBoardBanner renders a compact, dismissable onboarding strip
// above the column grid when the board has zero issues. Dismissal
// persists across reloads via localStorage so the chrome only nags on
// first encounter; the columns themselves stay visible regardless.
function EmptyBoardBanner({ onCreate }: { onCreate: () => void }) {
  const [dismissed, setDismissed] = useState(() => {
    if (typeof window === "undefined") return false;
    return window.localStorage.getItem(EMPTY_BANNER_DISMISSED_KEY) === "1";
  });
  if (dismissed) return null;
  const dismiss = () => {
    setDismissed(true);
    try {
      window.localStorage.setItem(EMPTY_BANNER_DISMISSED_KEY, "1");
    } catch {
      // localStorage may be unavailable (private mode, quota); the
      // in-memory state still hides the banner for this session.
    }
  };
  return (
    <div className="shrink-0 mx-3 mt-3 rounded border border-border-default bg-surface-1 p-3 text-sm text-fg-default flex items-start gap-3">
      <div className="flex-1 min-w-0">
        <div className="font-medium mb-0.5">Your kanban is empty</div>
        <div className="text-fg-muted text-xs leading-relaxed">
          Create your first issue (or press{" "}
          <kbd className="font-mono px-1 rounded bg-surface-2 border border-border-default">c</kbd>
          ) · Issues land in the first <em>eligible</em> column (green dot) ·
          Wire a dispatcher at{" "}
          <code className="text-xs bg-surface-2 px-1 rounded">/dispatcher</code>{" "}
          to auto-run workflows · Press{" "}
          <kbd className="font-mono px-1 rounded bg-surface-2 border border-border-default">?</kbd>{" "}
          for shortcuts
        </div>
      </div>
      <Button variant="primary" size="sm" onClick={onCreate}>
        + Create issue
      </Button>
      <button
        type="button"
        onClick={dismiss}
        className="text-fg-subtle hover:text-fg-default transition-colors leading-none text-lg px-1"
        title="Dismiss"
        aria-label="Dismiss onboarding banner"
      >
        ×
      </button>
    </div>
  );
}

function BoardFilters({
  searchQuery,
  labelFilter,
  assigneeFilter,
  allLabels,
  allAssignees,
  total,
  filtered,
  onSearchChange,
  onLabelToggle,
  onAssigneeChange,
  onReset,
}: {
  searchQuery: string;
  labelFilter: Set<string>;
  assigneeFilter: string;
  allLabels: string[];
  allAssignees: string[];
  total: number;
  filtered: number;
  onSearchChange: (v: string) => void;
  onLabelToggle: (l: string) => void;
  onAssigneeChange: (v: string) => void;
  onReset: () => void;
}) {
  const filtersActive =
    searchQuery.trim() !== "" || labelFilter.size > 0 || assigneeFilter !== "";
  return (
    <div className="px-3 py-2 border-b border-border-default bg-surface-1 flex flex-wrap items-center gap-2 text-xs">
      <input
        type="search"
        value={searchQuery}
        onChange={(e) => onSearchChange(e.target.value)}
        placeholder="Search title / body / id…"
        className="px-2 py-1 rounded border border-border-default bg-surface-0 text-fg-default text-xs min-w-[200px] flex-shrink-0"
      />
      {allAssignees.length > 0 && (
        <select
          value={assigneeFilter}
          onChange={(e) => onAssigneeChange(e.target.value)}
          className="px-2 py-1 rounded border border-border-default bg-surface-0 text-fg-default text-xs"
        >
          <option value="">All assignees</option>
          {allAssignees.map((a) => (
            <option key={a} value={a}>
              @{a}
            </option>
          ))}
        </select>
      )}
      {allLabels.length > 0 && (
        <div className="flex flex-wrap gap-1 items-center">
          <span className="text-fg-muted">labels:</span>
          {allLabels.map((l) => {
            const active = labelFilter.has(l);
            return (
              <button
                key={l}
                type="button"
                onClick={() => onLabelToggle(l)}
                className={`px-1.5 py-0.5 rounded border text-[10px] ${
                  active
                    ? "bg-accent text-fg-onAccent border-accent"
                    : "bg-surface-0 text-fg-muted border-border-default hover:text-fg-default"
                }`}
              >
                {l}
              </button>
            );
          })}
        </div>
      )}
      <span className="ml-auto text-fg-muted">
        {filtersActive ? `${filtered} / ${total}` : `${total} issue${total === 1 ? "" : "s"}`}
      </span>
      {filtersActive && (
        <button
          type="button"
          onClick={onReset}
          className="text-fg-subtle hover:text-fg-default underline text-[10px]"
        >
          reset
        </button>
      )}
    </div>
  );
}

function BoardKeyboardHelp({ onClose }: { onClose: () => void }) {
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === "Escape" || e.key === "?") {
        e.preventDefault();
        onClose();
      }
    };
    window.addEventListener("keydown", handler);
    return () => window.removeEventListener("keydown", handler);
  }, [onClose]);

  return (
    <div
      className="fixed inset-0 z-[var(--z-modal)] bg-black/40 flex items-center justify-center"
      onClick={onClose}
    >
      <div
        className="bg-surface-1 border border-border-default rounded shadow-lg p-5 max-w-sm text-sm"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="font-semibold text-fg-default mb-3">
          Keyboard shortcuts
        </div>
        <ul className="space-y-1.5 text-fg-default">
          <ShortcutRow keys="c / n" desc="New issue" />
          <ShortcutRow keys="↑ ↓" desc="Navigate cards in column" />
          <ShortcutRow keys="← →" desc="Move card to previous/next column" />
          <ShortcutRow keys="Enter / e" desc="Open selected issue" />
          <ShortcutRow keys="Del / Bksp" desc="Delete selected issue" />
          <ShortcutRow keys="Esc" desc="Clear selection or close" />
          <ShortcutRow keys="?" desc="Toggle this help" />
        </ul>
        <button
          type="button"
          onClick={onClose}
          className="mt-4 text-xs text-fg-subtle hover:text-fg-default"
        >
          Close
        </button>
      </div>
    </div>
  );
}

function ShortcutRow({ keys, desc }: { keys: string; desc: string }) {
  return (
    <li className="flex items-center justify-between gap-4">
      <kbd className="font-mono text-xs px-1.5 py-0.5 rounded bg-surface-2 border border-border-default">
        {keys}
      </kbd>
      <span className="text-fg-muted text-xs">{desc}</span>
    </li>
  );
}

// ---------------------------------------------------------------------------
// Column + Card
// ---------------------------------------------------------------------------

// defaultStateColor maps the conventional native-tracker state names
// (backlog/ready/in_progress/review/done/blocked) to a sensible palette
// so columns are scannable out of the box. Custom states fall back to a
// semantic colour from the eligible/terminal flags; truly unknown states
// get a neutral slate. Custom boards can always override per-state via
// the `color:` field — this helper only fires when `State.Color` is
// empty.
// TERMINAL_BOARD_STATES lists the native-tracker state names treated as
// "no more work" for UI purposes. The runtime contract is that any
// state with `terminal: true` in the board config qualifies — but the
// card doesn't carry the board's flag here, so we hard-code the
// canonical names. Keep in sync with the defaults in
// pkg/dispatcher/native/board.go's NewStore (done + blocked + cancelled).
const TERMINAL_BOARD_STATES = new Set(["done", "blocked", "cancelled"]);

function defaultStateColor(name: string, eligible: boolean, terminal: boolean): string {
  switch (name) {
    case "backlog":
      return "var(--color-board-backlog)";
    case "ready":
      return "var(--color-board-ready)";
    case "in_progress":
      return "var(--color-board-in-progress)";
    case "review":
      return "var(--color-board-review)";
    case "done":
      return "var(--color-board-done)";
    case "blocked":
      return "var(--color-board-blocked)";
    default:
      if (terminal) return "var(--color-board-done)";
      if (eligible) return "var(--color-board-ready)";
      return "var(--color-board-backlog)";
  }
}

interface ColumnProps {
  name: string;
  display: string;
  terminal: boolean;
  eligible: boolean;
  // Hex or CSS color string used to tint the column header strip and the
  // count chip. Always provided by the parent — either from State.Color
  // (board config) or from `defaultStateColor()` (semantic fallback).
  color: string;
  issues: NativeIssue[];
  selectedId: string | null;
  runningByIssue: Map<string, RunningView>;
  retryingByIssue: Map<string, RetryView>;
  onDrop: (issueID: string, toState: string) => void;
  onSelectCard: (id: string | null) => void;
  onClickCard: (iss: NativeIssue) => void;
  onLabelClick: (label: string) => void;
  activeLabels: Set<string>;
  onCancelRun: (issueID: string) => void;
  onOpenRun: (runId: string) => void;
  // dimmed: tells the column to render at reduced opacity. Used when the
  // dispatcher is paused so eligible columns visually fade — the cards
  // are still draggable, but the user gets a clear "nothing will pick
  // these up" signal.
  dimmed?: boolean;
}

function Column({
  name,
  display,
  terminal,
  eligible,
  color,
  issues,
  selectedId,
  runningByIssue,
  retryingByIssue,
  onDrop,
  onSelectCard,
  onClickCard,
  onLabelClick,
  activeLabels,
  onCancelRun,
  onOpenRun,
  dimmed,
}: ColumnProps) {
  const [dragOver, setDragOver] = useState(false);
  // Dim only the eligible columns when the dispatcher is paused — the
  // terminal / backlog columns aren't being actively dispatched even
  // when the dispatcher runs, so muting them carries no extra signal.
  const fadeForPause = dimmed && eligible;
  return (
    <div
      className={`w-72 shrink-0 rounded border-2 transition-colors ${
        dragOver
          ? "border-accent bg-accent-soft/30 ring-2 ring-accent/40"
          : "border-border-default bg-surface-1"
      } flex flex-col ${fadeForPause ? "opacity-60" : ""}`}
      style={{ borderTopColor: color, borderTopWidth: 3 }}
      onDragOver={(e) => {
        e.preventDefault();
        setDragOver(true);
      }}
      onDragLeave={() => setDragOver(false)}
      onDrop={(e) => {
        e.preventDefault();
        setDragOver(false);
        const id = e.dataTransfer.getData("text/plain");
        if (id && name !== "__unmapped__") onDrop(id, name);
      }}
    >
      <div className="px-3 py-2 border-b border-border-default flex items-center justify-between text-xs">
        <span className="flex items-center gap-2 min-w-0">
          <span
            className="inline-block h-2 w-2 rounded-full shrink-0"
            style={{ backgroundColor: color }}
            aria-hidden="true"
          />
          <span className="font-semibold uppercase tracking-wide text-fg-default truncate">
            {display}
          </span>
        </span>
        <span className="text-fg-muted">
          {issues.length}
          {eligible && <span className="ml-1 text-emerald-400">●</span>}
          {terminal && <span className="ml-1 text-fg-muted">✓</span>}
        </span>
      </div>
      <div className="p-2 flex-1 flex flex-col gap-2 overflow-auto">
        {issues.map((iss) => (
          <IssueCard
            key={iss.id}
            iss={iss}
            selected={iss.id === selectedId}
            running={runningByIssue.get(iss.id)}
            retrying={retryingByIssue.get(iss.id)}
            activeLabels={activeLabels}
            onSelect={() => onSelectCard(iss.id)}
            onClick={() => onClickCard(iss)}
            onLabelClick={onLabelClick}
            onCancelRun={() => onCancelRun(iss.id)}
            onOpenRun={onOpenRun}
            onShowRetryDetails={() => onClickCard(iss)}
          />
        ))}
        {issues.length === 0 && (
          <div className="text-xs text-fg-muted text-center py-4">drop here</div>
        )}
      </div>
    </div>
  );
}

interface IssueCardProps {
  iss: NativeIssue;
  selected: boolean;
  running?: RunningView;
  retrying?: RetryView;
  // activeLabels: the set of labels currently in the board-level
  // filter, so each card's label chip can show its active state and
  // operators can see which chips already filter the view.
  activeLabels: Set<string>;
  onSelect: () => void;
  onClick: () => void;
  onLabelClick: (label: string) => void;
  onCancelRun: () => void;
  onOpenRun: (runId: string) => void;
  onShowRetryDetails: () => void;
}

function IssueCard({
  iss,
  selected,
  running,
  retrying,
  activeLabels,
  onSelect,
  onClick,
  onLabelClick,
  onCancelRun,
  onOpenRun,
  onShowRetryDetails,
}: IssueCardProps) {
  // Hover preview: synthesise a multi-line title combining body
  // (truncated) + key fields + blocker count so the OS-native tooltip
  // provides a quick peek without forcing a modal open. Title strings
  // render with newlines on all major browsers.
  const previewLines: string[] = [];
  if (iss.body) {
    const trimmed = iss.body.trim();
    previewLines.push(trimmed.length > 240 ? trimmed.slice(0, 237) + "…" : trimmed);
  }
  if (iss.fields && Object.keys(iss.fields).length > 0) {
    previewLines.push(
      Object.entries(iss.fields)
        .map(([k, v]) => `${k}: ${String(v)}`)
        .join("\n"),
    );
  }
  if ((iss.blockers?.length ?? 0) > 0) {
    previewLines.push(`Blocked by: ${iss.blockers!.join(", ")}`);
  }
  const hoverTitle = previewLines.length > 0 ? previewLines.join("\n\n") : undefined;
  const [dragging, setDragging] = useState(false);
  const pinnedFields = iss.fields ? pickPinnedFields(iss.fields) : [];
  return (
    <div
      role="button"
      draggable
      title={hoverTitle}
      onDragStart={(e) => {
        e.dataTransfer.setData("text/plain", iss.id);
        e.dataTransfer.effectAllowed = "move";
        setDragging(true);
        onSelect();
      }}
      onDragEnd={() => setDragging(false)}
      onClick={() => {
        // Single click opens the modal directly — file-manager
        // double-click idioms confused operators ("I clicked it,
        // nothing happened") because there's no in-card affordance
        // distinguishing select from open. Selection still happens
        // as a side-effect so keyboard nav has an anchor when the
        // modal closes.
        onSelect();
        onClick();
      }}
      className={`bg-surface-0 border rounded p-2 text-sm cursor-grab active:cursor-grabbing transition-transform ${
        dragging ? "scale-[1.02] shadow-lg" : ""
      } ${
        selected
          ? "border-accent ring-1 ring-accent/40"
          : "border-border-default hover:border-accent/40"
      }`}
    >
      <div className="flex items-start gap-2">
        <span className="text-fg-default flex-1">{iss.title}</span>
        {iss.priority && iss.priority > 0 ? (
          <span className="text-[10px] px-1.5 py-0.5 rounded bg-amber-500/15 text-amber-300">
            P{iss.priority}
          </span>
        ) : null}
      </div>
      {pinnedFields.length > 0 && (
        <div className="mt-0.5 flex items-center gap-2 text-[10px] text-fg-subtle flex-wrap">
          {pinnedFields.map(([k, v]) => (
            <span key={k} className="flex items-center gap-1">
              <span className="font-mono opacity-70">{k}:</span>
              <span className="text-fg-default">{String(v)}</span>
            </span>
          ))}
        </div>
      )}
      {(iss.labels?.length ?? 0) > 0 && (
        <div className="mt-1 flex flex-wrap gap-1">
          {iss.labels!.map((l) => {
            const palette = labelPalette(l);
            const active = activeLabels.has(l);
            return (
              <button
                key={l}
                type="button"
                // Stop propagation so a chip click only toggles the
                // board's label filter — without this the card's
                // onClick would also open the issue modal, which is
                // not what the operator asked for.
                onClick={(e) => {
                  e.stopPropagation();
                  onLabelClick(l);
                }}
                className={`text-[10px] px-1.5 py-0.5 rounded hover:ring-1 hover:ring-accent transition ${
                  active ? "ring-1 ring-accent" : ""
                }`}
                style={palette}
                title={
                  active
                    ? `Click to remove ${l} from the board filter`
                    : `Click to filter board by ${l}`
                }
              >
                {l}
              </button>
            );
          })}
        </div>
      )}
      <div className="mt-1 flex items-center gap-2 text-[10px] text-fg-muted flex-wrap">
        <code className="opacity-70">{shortID(iss.id)}</code>
        {iss.bot && (
          <span
            className="font-mono bg-accent/15 text-accent rounded px-1 py-0.5"
            title={`Will dispatch via ${iss.bot} (overrides dispatcher config)`}
          >
            🤖 {iss.bot}
          </span>
        )}
        {iss.assignee && <span>@{iss.assignee}</span>}
        {iss.claim && (
          <span
            className="text-amber-300"
            title={`Locked by ${iss.claim} — the dispatcher holds the claim until the run finishes.`}
          >
            claimed by {iss.claim}
          </span>
        )}
        {!running && iss.last_run_id && (
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onOpenRun(iss.last_run_id!);
            }}
            className="font-mono text-info hover:underline opacity-80"
            title={`Open the last run on this issue (run ${iss.last_run_id})`}
          >
            ↪ last run
          </button>
        )}
        {iss.updated_at && (
          <span className="text-fg-subtle" title={iss.updated_at}>
            · updated {formatRelative(iss.updated_at)}
          </span>
        )}
      </div>
      {running && (
        <div className="mt-1 flex items-center justify-between gap-2 rounded bg-green-500/10 px-1.5 py-1 text-[10px] text-green-300">
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onOpenRun(running.run_id);
            }}
            className="text-left flex-1 hover:underline cursor-pointer"
            title={
              running.attempt && running.attempt > 0
                ? `Open run ${running.run_id} (resume of a prior failed_resumable run — attempt ${running.attempt + 1})`
                : `Open run ${running.run_id}`
            }
          >
            ● {running.attempt && running.attempt > 0 ? "resuming" : "running"}
            {running.attempt && running.attempt > 0 ? (
              <span className="ml-1 text-amber-300/90">#{running.attempt + 1}</span>
            ) : null}
            {running.last_event_name && (
              <span className="ml-1 text-green-200/70">— {running.last_event_name}</span>
            )}
          </button>
          <button
            className="rounded border border-green-500/40 px-1.5 py-0.5 text-[10px] hover:bg-green-500/20"
            onClick={(e) => {
              e.stopPropagation();
              onCancelRun();
            }}
            title="Cancel this in-flight run"
          >
            cancel
          </button>
        </div>
      )}
      {!running && retrying && !TERMINAL_BOARD_STATES.has(iss.state) && (
        <div
          className="mt-1 rounded bg-amber-500/10 px-1.5 py-1 text-[10px] text-amber-300 cursor-pointer hover:bg-amber-500/20"
          onClick={(e) => {
            e.stopPropagation();
            onShowRetryDetails();
          }}
          title={retrying.error ? `Last error: ${retrying.error}` : undefined}
        >
          ⏳ retrying (attempt {retrying.attempt})
          {retrying.error && (
            <span className="ml-1 text-amber-200/80 truncate">— {retrying.error}</span>
          )}
        </div>
      )}
      {!running && retrying && TERMINAL_BOARD_STATES.has(iss.state) && (
        <div
          className="mt-1 rounded bg-fg-muted/10 px-1.5 py-1 text-[10px] text-fg-subtle"
          title={`The dispatcher still has a retry entry for this issue, but it's in a terminal state (${iss.state}) — the retry will be skipped on the next tick.`}
        >
          stale retry queued — will be skipped (issue in {iss.state})
        </div>
      )}
    </div>
  );
}

// labelPalette derives a stable pastel background + foreground colour
// from a label name. Two cards with the label "urgent" always render
// the same colour, but "infra" and "urgent" land on visibly distinct
// palettes. Hashing avoids the need for a label-colour schema in the
// backend — operators get colour scanning today without configuration.
// A small alias table covers common semantic labels with sensible
// presets (red for "urgent" / "bug", green for "ready", etc.).
function labelPalette(label: string): { backgroundColor: string; color: string } {
  const aliases: Record<string, { backgroundColor: string; color: string }> = {
    urgent: { backgroundColor: "rgba(239,68,68,0.18)", color: "#fecaca" },
    blocker: { backgroundColor: "rgba(239,68,68,0.18)", color: "#fecaca" },
    bug: { backgroundColor: "rgba(244,63,94,0.18)", color: "#fecdd3" },
    infra: { backgroundColor: "rgba(59,130,246,0.18)", color: "#bfdbfe" },
    docs: { backgroundColor: "rgba(168,85,247,0.18)", color: "#e9d5ff" },
    feature: { backgroundColor: "rgba(16,185,129,0.18)", color: "#bbf7d0" },
    ready: { backgroundColor: "rgba(16,185,129,0.18)", color: "#bbf7d0" },
  };
  const hit = aliases[label.toLowerCase()];
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
function pickPinnedFields(fields: Record<string, unknown>): Array<[string, unknown]> {
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


function shortID(id: string) {
  const bare = id.replace(/^native:/, "").replace(/^github:[^#]+#/, "#");
  return bare.length > 10 ? bare.slice(0, 10) : bare;
}
