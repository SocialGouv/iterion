import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "wouter";

import BranchDiffModal from "@/components/Runs/BranchDiffModal";
import HumanPromptForm from "@/components/Runs/conversation/HumanPromptForm";
import { getRun } from "@/api/runs";
import { rehydratePendingHumanInput } from "@/store/run/reducer";
import { Button } from "@/components/ui/Button";
import { Badge } from "@/components/ui/Badge";
import { CopyButton } from "@/components/ui/CopyButton";
import { useRunChildren } from "@/hooks/useRunChildren";
import { childLabel } from "@/components/Runs/runHeader/RunChildrenPanel";
import { useServerInfoStore } from "@/store/serverInfo";
import {
  STATUS_VARIANT,
  labelForStatus,
} from "@/components/Runs/runStatusMeta";
import type { RunRef } from "@/api/native";

// ChildrenDisclosure lets a run row in the card history expand to its
// shard/fork subtree — so an operator sees a card's pipeline tree
// without leaving the board. Lazy: the useRunChildren fetch is gated on
// `open`, so a collapsed history list makes ZERO children requests (no
// N+1 across the card's runs); the fetch only fires when the operator
// clicks a specific row open.
function ChildrenDisclosure({ runID }: { runID: string }) {
  const [open, setOpen] = useState(false);
  const { data: children } = useRunChildren(runID, open);
  return (
    <div className="text-xs">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="text-fg-muted hover:text-fg-default"
        title="Show this run's shard/fork children"
      >
        {open ? "▾" : "▸"} Children
        {open && children.length > 0 ? ` (${children.length})` : ""}
      </button>
      {open && children.length === 0 && (
        <span className="ml-1 text-fg-subtle">— none</span>
      )}
      {open && children.length > 0 && (
        <ul className="mt-1 space-y-1 pl-3">
          {children.map((c) => (
            <li key={c.id} className="flex items-center gap-1.5">
              <Link
                href={`/runs/${encodeURIComponent(c.id)}`}
                className="font-mono text-accent-text hover:underline"
                title={`Open child run ${c.id}`}
              >
                {childLabel(c)}
              </Link>
              <Badge variant={STATUS_VARIANT[c.status]} className="ml-auto">
                {labelForStatus(c.status)}
              </Badge>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// AwaitingInput reads the stamped last run and, when it is paused on a
// human node, renders the answer affordance inline on the card — so an
// operator can respond to a paused pipeline directly from the board
// instead of detouring through the run console (issue #125, point 4).
//
// It reuses HumanPromptForm — the SAME schema-driven form the run console
// renders — so a paused human node's real answer fields (its output
// schema: enums, checkboxes, text) show, not the checkpoint's context
// vars. rehydratePendingHumanInput (shared, runtime-narrows the opaque
// checkpoint, gates on paused_waiting_human) yields the paused node id +
// questions map the form needs. sourceOverride={null} sends NO source so
// the server resumes against the run's persisted FilePath (the operator
// isn't editing this run's workflow); onResumed refetches the board view
// instead of the run-console WS machinery.
function AwaitingInput({ runID }: { runID: string }) {
  const { data, refetch } = useQuery({
    queryKey: ["board-last-run", runID],
    queryFn: ({ signal }) => getRun(runID, { signal }),
    // Poll while the run can still transition into (or back out of) a pause,
    // stopping once it is terminal. A resume flips a paused card to running
    // and a later human node re-pauses it — polling through 'running'/'queued'
    // keeps the affordance live without reopening the modal. `enabled` can't
    // gate this; the first fetch is what reveals the status.
    refetchInterval: (q) => {
      const s = q.state.data?.run?.status;
      return s === "running" ||
        s === "queued" ||
        s === "paused_waiting_human" ||
        s === "paused_operator"
        ? 5000
        : false;
    },
  });
  const pending = data?.run ? rehydratePendingHumanInput(data) : null;
  if (!pending || !pending.node_id) return null;
  return (
    <div className="rounded border border-warning/40 bg-warning-soft p-2 space-y-2">
      <div className="text-micro uppercase tracking-wide text-warning-fg">
        ⏸ Awaiting input
      </div>
      <HumanPromptForm
        runId={runID}
        nodeId={pending.node_id}
        questions={pending.questions ?? {}}
        sourceOverride={null}
        onResumed={() => void refetch()}
      />
    </div>
  );
}

// RunPanel renders one run's surfaces (paused answer affordance, run
// console link, branch diff, worktree links). It is the single-run unit
// the run-history list repeats — and the same body the back-compat
// single-last-run fallback renders.
//
// Renders nothing when neither runID nor workdir is set.
function RunPanel({ runID, workdir }: { runID?: string; workdir?: string }) {
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const [diffOpen, setDiffOpen] = useState(false);
  if (!runID && !workdir) return null;
  const runLabel = runID ? runID.slice(0, 12) : "";
  return (
    <div className="space-y-1.5">
      {runID && <AwaitingInput runID={runID} />}
      {runID && (
        <div className="flex items-center gap-1.5 text-xs">
          <span className="text-fg-muted">Run:</span>
          <Link
            href={`/runs/${encodeURIComponent(runID)}`}
            className="font-mono text-accent-text hover:underline"
            title={`Open run ${runID}`}
          >
            {runLabel}
          </Link>
          <CopyButton value={runID} variant="icon" label="Copy run id" />
          <Button
            variant="secondary"
            size="sm"
            onClick={() => setDiffOpen(true)}
            className="ml-auto"
            title="View this run's full branch diff without leaving the board"
          >
            View diff
          </Button>
        </div>
      )}
      {runID && <ChildrenDisclosure runID={runID} />}
      {runID && (
        <BranchDiffModal
          runId={runID}
          open={diffOpen}
          onClose={() => setDiffOpen(false)}
        />
      )}
      {/* The workdir is a host filesystem path. In cloud it lives inside
          the runner pod — copying it or opening it in VS Code is pure
          misdirection, so the row is local/desktop-only. */}
      {workdir && !cloud && (
        <div className="flex items-center gap-1.5 text-xs">
          <span className="text-fg-muted">Worktree:</span>
          <code
            className="flex-1 min-w-0 truncate bg-surface-2 px-1 py-0.5 rounded text-micro"
            title={workdir}
          >
            {workdir}
          </code>
          <CopyButton value={workdir} variant="icon" label="Copy worktree path" />
          <a
            href={`vscode://file/${workdir}`}
            className="text-micro px-1.5 py-0.5 rounded border border-border-default hover:bg-surface-2 text-fg-default"
            title="Open the worktree in VS Code (vscode:// URL handler)"
          >
            VS Code
          </a>
        </div>
      )}
    </div>
  );
}

// LastRunSection renders the issue's run history inside the Ticket tab.
// When `runs` has entries it renders them as a LIST (newest-last, one
// RunPanel per row) so an operator sees every run that touched the card,
// with the paused-run answer affordance live on each. When `runs` is
// absent (records written before run history was tracked) it falls back
// to the single last-run pointer (runID/workdir).
//
// Renders nothing when there is no history and no single-run pointer;
// callers gate the mount on that condition too.
export function LastRunSection({
  runID,
  workdir,
  runs,
}: {
  runID?: string;
  workdir?: string;
  runs?: RunRef[];
}) {
  const hasHistory = runs != null && runs.length > 0;
  if (!hasHistory && !runID && !workdir) return null;
  return (
    <div className="rounded border border-border-default bg-surface-1 p-2 space-y-1.5">
      <div className="text-micro uppercase tracking-wide text-fg-subtle">
        {hasHistory && runs.length > 1 ? "Run history" : "Last run"}
      </div>
      {hasHistory ? (
        <div className="space-y-2">
          {runs.map((r, i) => (
            <div
              key={r.run_id || i}
              className={
                i > 0 ? "border-t border-border-default pt-2" : undefined
              }
            >
              <RunPanel runID={r.run_id} workdir={r.workdir} />
            </div>
          ))}
        </div>
      ) : (
        <RunPanel runID={runID} workdir={workdir} />
      )}
    </div>
  );
}
