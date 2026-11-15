// Extracted from api/runs.ts to keep that file focused.
// Run-tree children fetcher (T4b, refs #125): the reverse edge of the
// shard-tuple projection — every run whose parent_run_id points at this
// run. Mirrors the other run sub-resource fetchers (commits, files).

import { beginRowRequest } from "@/lib/lostRuns";
import { request } from "./client";
import { noteRunBirths } from "./rowBirths";
import type { RunSummary } from "./types";

// getRunChildren fetches the shard/child subtree of a run — the runs
// spawned with parent_run_id == runId — via GET /api/runs/{id}/children.
// Returns an empty array for a run with no children (a top-level or
// leaf run), so callers can render a collapsed/absent panel without a
// null guard. The request reads the daemon's own store, where the child
// rows' births are noted: a child opened from one expects it.
export async function getRunChildren(runId: string): Promise<RunSummary[]> {
  const seq = beginRowRequest();
  const res = await request<{ runs: RunSummary[] }>(
    `/runs/${encodeURIComponent(runId)}/children`,
  );
  return noteRunBirths("", res.runs ?? [], seq);
}
