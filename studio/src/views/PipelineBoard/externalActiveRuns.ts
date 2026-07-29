import type { GlobalActiveRun, RunStatus } from "@/api/runs";
import type { PipelineBoardCard } from "@/api/pipelineBoards";

const PIPELINE_LIVE_STATUSES = new Set<RunStatus>([
  "running",
  "queued",
  "paused_waiting_human",
  "paused_operator",
]);

// The pipeline board is projected from one run store, while the local
// daemon's global-active feed can see every store under ~/.iterion. A CLI
// run resumed with another --store-dir must therefore be surfaced
// separately instead of being confused with an older card in this board.
export function externalActiveRuns(
  runs: readonly GlobalActiveRun[],
  cards: readonly PipelineBoardCard[],
  projectDir: string | null,
): GlobalActiveRun[] {
  const boardRunIDs = new Set<string>();
  const boardWorkflows = new Set<string>();

  for (const card of cards) {
    if (card.run_id) boardRunIDs.add(card.run_id);
    for (const runID of card.tree_run_ids ?? []) boardRunIDs.add(runID);
    if (card.workflow_name) boardWorkflows.add(card.workflow_name);
  }

  return runs
    // The global endpoint also exposes failed_resumable records so Home can
    // offer recovery. An explicit allowlist prevents historical inventory
    // from drowning the pipeline board in old failures.
    .filter(
      (run) =>
        PIPELINE_LIVE_STATUSES.has(run.status) &&
        !run.parent_run_id &&
        !boardRunIDs.has(run.id) &&
        belongsToProject(run.workspace_dir, projectDir),
    )
    .sort((a, b) => {
      // Put likely alternate attempts of a workflow already on this board
      // first. Unrelated active work remains visible below it.
      const aRelated = boardWorkflows.has(a.workflow_name);
      const bRelated = boardWorkflows.has(b.workflow_name);
      if (aRelated !== bRelated) return aRelated ? -1 : 1;
      return Date.parse(b.updated_at) - Date.parse(a.updated_at);
    });
}

function belongsToProject(
  workspaceDir: string | undefined,
  projectDir: string | null,
): boolean {
  // Server-info/Desktop project identity arrives asynchronously. Suppress
  // the notice until it is known instead of briefly flashing runs from every
  // project on the machine.
  if (!projectDir) return false;
  if (!workspaceDir) return false;

  const project = trimTrailingSeparators(projectDir);
  const workspace = trimTrailingSeparators(workspaceDir);
  return workspace === project || workspace.startsWith(`${project}/`);
}

function trimTrailingSeparators(path: string): string {
  const normalized = path.replace(/\\/g, "/");
  const trimmed = normalized.replace(/\/+$/, "");
  return trimmed || normalized;
}

export function crossStoreRunHref(run: GlobalActiveRun): string {
  const params = new URLSearchParams({ store: run.store_path });
  return `/runs/${encodeURIComponent(run.id)}?${params.toString()}`;
}
