import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocation, useSearch } from "wouter";

import PageShell from "@/components/shared/PageShell";
import ConductorControlBar from "@/components/shared/ConductorControlBar";
import { Button } from "@/components/ui/Button";
import {
  cancelIssue,
  getState,
  type RetryView,
  type RunningView,
} from "@/api/conductor";
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
import SettingsDrawer from "@/views/Conductor/SettingsDrawer";
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
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [helpOpen, setHelpOpen] = useState(false);

  // Poll the conductor snapshot every 2s so each card can show a
  // running/retrying badge + cancel button. We ignore failures: when
  // the conductor is idle the snapshot is still returned (empty
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
        setTrackerError(
          snap.last_tracker_error
            ? { tracker: snap.tracker, message: snap.last_tracker_error }
            : null,
        );
      } catch {
        // swallow: conductor may be unreachable / not wired
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

  // Apply the ?focus=<issueID> deep-link from the Conductor view's
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

  // Group issues by state for column rendering. Issues whose state
  // does not appear on the board land in an "unmapped" bucket so they
  // are not silently lost when the operator renames a state.
  const byState = useMemo(() => {
    const m = new Map<string, NativeIssue[]>();
    if (!board) return m;
    for (const s of board.states) m.set(s.name, []);
    m.set("__unmapped__", []);
    for (const iss of issues) {
      const bucket = m.has(iss.state) ? iss.state : "__unmapped__";
      m.get(bucket)!.push(iss);
    }
    for (const list of m.values()) {
      list.sort((a, b) => (b.priority ?? 0) - (a.priority ?? 0));
    }
    return m;
  }, [board, issues]);

  const onDrop = useCallback(
    async (issueID: string, toState: string) => {
      // Capture this invocation's pre-state in a per-call closure so
      // two near-simultaneous drops don't race over the same `before`
      // variable. The prior implementation hoisted `before` to the
      // outer scope and the second drop would overwrite the first
      // drop's snapshot before its async transitionIssue had a chance
      // to fail / roll back, restoring the wrong row.
      const draft: { snapshot: NativeIssue[] } = { snapshot: [] };
      setIssues((cur) => {
        draft.snapshot = cur;
        return cur.map((i) => (i.id === issueID ? { ...i, state: toState } : i));
      });
      try {
        await transitionIssue(issueID, toState);
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
    [],
  );

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
      if (!confirm("Delete this issue?")) return;
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

  if (loading) {
    return (
      <PageShell active="board">
        <div className="p-8 text-fg-muted">Loading kanban…</div>
      </PageShell>
    );
  }
  if (!board) {
    return (
      <PageShell active="board">
        <div className="p-8 text-fg-muted">
          Native tracker not available.{" "}
          <code className="text-xs">iterion editor --dir &lt;project&gt;</code> creates one on first launch.
        </div>
      </PageShell>
    );
  }

  return (
    <PageShell
      active="board"
      rightActions={
        <>
          <Button variant="secondary" size="sm" onClick={() => void refresh()}>
            Refresh
          </Button>
          <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
            + New issue
          </Button>
        </>
      }
    >

      <ConductorControlBar onOpenSettings={() => setSettingsOpen(true)} />
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
        <div className="bg-amber-500/10 border-b border-amber-500/40 px-4 py-2 text-xs text-amber-200 flex items-start gap-2">
          <span className="font-medium shrink-0">Tracker error:</span>
          <span className="font-mono break-words flex-1">{trackerError.message}</span>
          <span className="text-amber-200/60 shrink-0">({trackerError.tracker})</span>
        </div>
      )}

      <main className="flex-1 overflow-auto p-3">
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
              onCancelRun={onCancelRun}
              onOpenRun={(runId) => setLocation(`/runs/${encodeURIComponent(runId)}`)}
            />
          ))}
          {(byState.get("__unmapped__")?.length ?? 0) > 0 && (
            <Column
              name="__unmapped__"
              display="Unmapped"
              terminal={false}
              eligible={false}
              color="#64748b"
              issues={byState.get("__unmapped__") ?? []}
              selectedId={selectedId}
              runningByIssue={runningByIssue}
              retryingByIssue={retryingByIssue}
              onDrop={onDrop}
              onSelectCard={setSelectedId}
              onClickCard={(iss) => setEditing(iss)}
              onCancelRun={onCancelRun}
              onOpenRun={(runId) => setLocation(`/runs/${encodeURIComponent(runId)}`)}
            />
          )}
        </div>
      </main>

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
    </PageShell>
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
      className="fixed inset-0 z-50 bg-black/40 flex items-center justify-center"
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
function defaultStateColor(name: string, eligible: boolean, terminal: boolean): string {
  switch (name) {
    case "backlog":
      return "#64748b"; // slate-500
    case "ready":
      return "#22c55e"; // green-500
    case "in_progress":
      return "#3b82f6"; // blue-500
    case "review":
      return "#a855f7"; // purple-500
    case "done":
      return "#94a3b8"; // slate-400 (terminal success)
    case "blocked":
      return "#ef4444"; // red-500
    default:
      if (terminal) return "#94a3b8";
      if (eligible) return "#22c55e";
      return "#64748b";
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
  onCancelRun: (issueID: string) => void;
  onOpenRun: (runId: string) => void;
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
  onCancelRun,
  onOpenRun,
}: ColumnProps) {
  const [dragOver, setDragOver] = useState(false);
  return (
    <div
      className={`w-72 shrink-0 rounded border ${
        dragOver ? "border-accent/60 bg-accent-soft/30" : "border-border-default bg-surface-1"
      } flex flex-col`}
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
            onSelect={() => onSelectCard(iss.id)}
            onClick={() => onClickCard(iss)}
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
  onSelect: () => void;
  onClick: () => void;
  onCancelRun: () => void;
  onOpenRun: (runId: string) => void;
  onShowRetryDetails: () => void;
}

function IssueCard({
  iss,
  selected,
  running,
  retrying,
  onSelect,
  onClick,
  onCancelRun,
  onOpenRun,
  onShowRetryDetails,
}: IssueCardProps) {
  return (
    <div
      role="button"
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData("text/plain", iss.id);
        e.dataTransfer.effectAllowed = "move";
        onSelect();
      }}
      onClick={(e) => {
        // Single click selects (so keyboard nav has an anchor); a second
        // click on the already-selected card opens the modal — mirroring
        // file-manager double-click idioms but with a much shorter delay.
        if (selected) {
          onClick();
        } else {
          e.preventDefault();
          onSelect();
        }
      }}
      onDoubleClick={onClick}
      className={`bg-surface-0 border rounded p-2 text-sm cursor-grab active:cursor-grabbing ${
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
      {(iss.labels?.length ?? 0) > 0 && (
        <div className="mt-1 flex flex-wrap gap-1">
          {iss.labels!.map((l) => (
            <span
              key={l}
              className="text-[10px] px-1.5 py-0.5 rounded bg-surface-2 text-fg-muted"
            >
              {l}
            </span>
          ))}
        </div>
      )}
      <div className="mt-1 flex items-center gap-2 text-[10px] text-fg-muted">
        <code className="opacity-70">{shortID(iss.id)}</code>
        {iss.assignee && <span>@{iss.assignee}</span>}
        {iss.claim && <span className="text-amber-300">claimed</span>}
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
            title={`Open run ${running.run_id}`}
          >
            ● running
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
      {!running && retrying && (
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
    </div>
  );
}

function shortID(id: string) {
  const bare = id.replace(/^native:/, "").replace(/^github:[^#]+#/, "#");
  return bare.length > 10 ? bare.slice(0, 10) : bare;
}
