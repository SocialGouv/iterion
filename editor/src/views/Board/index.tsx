import { useCallback, useEffect, useMemo, useState } from "react";

import ConductorControlBar from "@/components/shared/ConductorControlBar";
import NavLinks from "@/components/shared/NavLinks";
import ProjectLabel from "@/components/shared/ProjectLabel";
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

export default function BoardView() {
  const [board, setBoard] = useState<NativeBoard | null>(null);
  const [issues, setIssues] = useState<NativeIssue[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [editing, setEditing] = useState<NativeIssue | null>(null);
  const [creating, setCreating] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [runningByIssue, setRunningByIssue] = useState<Map<string, RunningView>>(new Map());
  const [retryingByIssue, setRetryingByIssue] = useState<Map<string, RetryView>>(new Map());

  // Poll the conductor snapshot every 2s so each card can show a
  // running/retrying badge + cancel button. We ignore failures: when
  // the conductor is idle the snapshot is still returned (empty
  // running/retries), and a 5xx is rare enough that flashing the maps
  // empty would be more disruptive than keeping stale data.
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      try {
        const snap = await getState();
        if (!alive) return;
        const rmap = new Map<string, RunningView>();
        for (const r of snap.running ?? []) rmap.set(r.issue_id, r);
        const xmap = new Map<string, RetryView>();
        for (const r of snap.retries ?? []) xmap.set(r.issue_id, r);
        setRunningByIssue(rmap);
        setRetryingByIssue(xmap);
      } catch {
        // swallow: conductor may be unreachable / not wired
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
      const before = issues;
      // optimistic update
      setIssues((cur) => cur.map((i) => (i.id === issueID ? { ...i, state: toState } : i)));
      try {
        await transitionIssue(issueID, toState);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setIssues(before);
      }
    },
    [issues],
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
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    },
    [refresh],
  );

  if (loading) {
    return <div className="p-8 text-fg-muted">Loading kanban…</div>;
  }
  if (!board) {
    return (
      <div className="p-8 text-fg-muted">
        Native tracker not available.{" "}
        <code className="text-xs">iterion editor --dir &lt;project&gt;</code> creates one on first launch.
      </div>
    );
  }

  return (
    <div className="h-full flex flex-col overflow-hidden bg-surface-0 text-fg-default">
      <header className="border-b border-border-default px-4 py-2.5 flex items-center gap-3 bg-surface-1">
        <span className="text-sm font-bold tracking-wide">ITERION</span>
        <NavLinks active="board" />
        <ProjectLabel />
        <div className="ml-auto flex items-center gap-2">
          <button
            className="text-xs px-2 py-1 rounded border border-border-default hover:bg-surface-2"
            onClick={() => void refresh()}
          >
            Refresh
          </button>
          <button
            className="text-xs px-2 py-1 rounded bg-accent text-on-accent hover:opacity-90"
            onClick={() => setCreating(true)}
          >
            + New issue
          </button>
        </div>
      </header>

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

      <main className="flex-1 overflow-auto p-3">
        <div className="flex gap-3 min-w-fit">
          {board.states.map((s) => (
            <Column
              key={s.name}
              name={s.name}
              display={s.display ?? s.name}
              terminal={!!s.terminal}
              eligible={!!s.eligible}
              issues={byState.get(s.name) ?? []}
              runningByIssue={runningByIssue}
              retryingByIssue={retryingByIssue}
              onDrop={onDrop}
              onClickCard={(iss) => setEditing(iss)}
              onCancelRun={onCancelRun}
            />
          ))}
          {(byState.get("__unmapped__")?.length ?? 0) > 0 && (
            <Column
              name="__unmapped__"
              display="Unmapped"
              terminal={false}
              eligible={false}
              issues={byState.get("__unmapped__") ?? []}
              runningByIssue={runningByIssue}
              retryingByIssue={retryingByIssue}
              onDrop={onDrop}
              onClickCard={(iss) => setEditing(iss)}
              onCancelRun={onCancelRun}
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
    </div>
  );
}

// ---------------------------------------------------------------------------
// Column + Card
// ---------------------------------------------------------------------------

interface ColumnProps {
  name: string;
  display: string;
  terminal: boolean;
  eligible: boolean;
  issues: NativeIssue[];
  runningByIssue: Map<string, RunningView>;
  retryingByIssue: Map<string, RetryView>;
  onDrop: (issueID: string, toState: string) => void;
  onClickCard: (iss: NativeIssue) => void;
  onCancelRun: (issueID: string) => void;
}

function Column({
  name,
  display,
  terminal,
  eligible,
  issues,
  runningByIssue,
  retryingByIssue,
  onDrop,
  onClickCard,
  onCancelRun,
}: ColumnProps) {
  const [dragOver, setDragOver] = useState(false);
  return (
    <div
      className={`w-72 shrink-0 rounded border ${
        dragOver ? "border-accent/60 bg-accent-soft/30" : "border-border-default bg-surface-1"
      } flex flex-col`}
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
        <span className="font-semibold uppercase tracking-wide text-fg-default">{display}</span>
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
            running={runningByIssue.get(iss.id)}
            retrying={retryingByIssue.get(iss.id)}
            onClick={() => onClickCard(iss)}
            onCancelRun={() => onCancelRun(iss.id)}
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
  running?: RunningView;
  retrying?: RetryView;
  onClick: () => void;
  onCancelRun: () => void;
}

function IssueCard({ iss, running, retrying, onClick, onCancelRun }: IssueCardProps) {
  return (
    <div
      role="button"
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData("text/plain", iss.id);
        e.dataTransfer.effectAllowed = "move";
      }}
      onClick={onClick}
      className="bg-surface-0 border border-border-default rounded p-2 text-sm cursor-grab hover:border-accent/40 active:cursor-grabbing"
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
          <span>
            ● running
            {running.last_event_name && (
              <span className="ml-1 text-green-200/70">— {running.last_event_name}</span>
            )}
          </span>
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
        <div className="mt-1 rounded bg-amber-500/10 px-1.5 py-1 text-[10px] text-amber-300">
          ⏳ retrying (attempt {retrying.attempt})
        </div>
      )}
    </div>
  );
}

function shortID(id: string) {
  const bare = id.replace(/^native:/, "").replace(/^github:[^#]+#/, "#");
  return bare.length > 10 ? bare.slice(0, 10) : bare;
}
