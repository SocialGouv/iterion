import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "wouter";

import BranchDiffModal from "@/components/Runs/BranchDiffModal";
import PauseForm from "@/components/Runs/PauseForm";
import { getRun } from "@/api/runs";
import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";

// AwaitingInput reads the stamped last run and, when it is paused on a
// human node, renders the answer affordance inline on the card — so an
// operator can respond to a paused pipeline directly from the board
// instead of detouring through the run console (issue #125, point 4).
//
// It reuses PauseForm verbatim: the paused questions live on the run
// checkpoint (store.Checkpoint.InteractionQuestions, surfaced through the
// snapshot's opaque checkpoint index signature), the same data PauseForm
// consumes in the run console. sourceOverride={null} is load-bearing: the
// operator isn't editing this run's workflow here, so the resume must
// carry NO source and let the server fall back to the run's persisted
// FilePath (passing the editor buffer would resume an unrelated workflow).
function AwaitingInput({ runID }: { runID: string }) {
  const { data, refetch } = useQuery({
    queryKey: ["board-last-run", runID],
    queryFn: ({ signal }) => getRun(runID, { signal }),
    // Poll gently: a parked run flips to running on resume and back if it
    // re-pauses; a light refetch keeps the affordance honest without a WS.
    refetchInterval: 5000,
  });
  const run = data?.run;
  const paused =
    run?.status === "paused_waiting_human" || run?.status === "paused_operator";
  if (!paused) return null;
  const questions =
    (run?.checkpoint?.interaction_questions as Record<string, unknown> | undefined) ?? {};
  return (
    <div className="rounded border border-warning/40 bg-warning-soft p-2 space-y-2">
      <div className="text-micro uppercase tracking-wide text-warning-fg">
        ⏸ Awaiting input
      </div>
      <PauseForm
        runId={runID}
        questions={questions}
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
