import { useState } from "react";
import { Link } from "wouter";

import BranchDiffModal from "@/components/Runs/BranchDiffModal";
import { Button } from "@/components/ui/Button";
import { CopyButton } from "@/components/ui/CopyButton";

// LastRunSection renders a compact "Last run" panel inside the
// Ticket tab when the dispatcher has stamped a run on the issue.
// Surfaces:
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
