import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "wouter";

import BranchDiffModal from "@/components/Runs/BranchDiffModal";
import PauseForm from "@/components/Runs/PauseForm";
import { getRun } from "@/api/runs";
import { rehydratePendingHumanInput } from "@/store/run/reducer";
import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";

// AwaitingInput reads the stamped last run and, when it is paused on a
// human node, renders the answer affordance inline on the card — so an
// operator can respond to a paused pipeline directly from the board
// instead of detouring through the run console (issue #125, point 4).
//
// It reuses PauseForm verbatim, decoding the paused node's questions with
// the shared rehydratePendingHumanInput (which runtime-narrows the opaque
// run checkpoint and gates on paused_waiting_human) — the same helper the
// run console uses to rebuild the pause panel after a reload.
// sourceOverride={null} is load-bearing: the operator isn't editing this
// run's workflow here, so the resume must carry NO source and let the
// server fall back to the run's persisted FilePath (passing the editor
// buffer would resume an unrelated workflow).
function AwaitingInput({ runID }: { runID: string }) {
  const { data, refetch } = useQuery({
    queryKey: ["board-last-run", runID],
    queryFn: ({ signal }) => getRun(runID, { signal }),
    // Poll only while genuinely paused: one fetch to learn the status, then
    // a light refetch that stops once the run resumes/terminates (a parked
    // run flips to running on answer). `enabled` can't gate this — the first
    // fetch is what reveals the status.
    refetchInterval: (q) =>
      q.state.data?.run?.status === "paused_waiting_human" ? 5000 : false,
  });
  const pending = data ? rehydratePendingHumanInput(data) : null;
  if (!pending) return null;
  return (
    <div className="rounded border border-warning/40 bg-warning-soft p-2 space-y-2">
      <div className="text-micro uppercase tracking-wide text-warning-fg">
        ⏸ Awaiting input
      </div>
      <PauseForm
        runId={runID}
        questions={pending.questions ?? {}}
        sourceOverride={null}
        onSubmitted={() => void refetch()}
      />
    </div>
  );
}

// LastRunSection renders a compact "Last run" panel inside the
// Ticket tab when the dispatcher has stamped a run on the issue.
// Surfaces:
//   - An inline answer affordance when the run is paused on a human node.
//   - A wouter Link to the run console at /runs/<id>.
//   - The worktree path with copy-to-clipboard and vscode:// links
//     so the operator can pivot from the kanban card into a diff
//     inspector without leaving the studio.
//
// Renders nothing when neither runID nor workdir is set; callers
// gate the mount on that condition too.
export function LastRunSection({
  runID,
  workdir,
}: {
  runID?: string;
  workdir?: string;
}) {
  const [diffOpen, setDiffOpen] = useState(false);
  if (!runID && !workdir) return null;
  const runLabel = runID ? runID.slice(0, 12) : "";
  return (
    <div className="rounded border border-border-default bg-surface-1 p-2 space-y-1.5">
      {runID && <AwaitingInput runID={runID} />}
      <div className="text-micro uppercase tracking-wide text-fg-subtle">
        Last run
      </div>
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
      {runID && (
        <BranchDiffModal
          runId={runID}
          open={diffOpen}
          onClose={() => setDiffOpen(false)}
        />
      )}
      {workdir && (
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
