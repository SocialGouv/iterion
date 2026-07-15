// Extracted from api/runs.ts to keep that file focused.
// Run-tree children fetcher (T4b, refs #125): the reverse edge of the
// shard-tuple projection — every run whose parent_run_id points at this
// run. Mirrors the other run sub-resource fetchers (commits, files).

import { request } from "./client";
import type { RunSummary } from "./types";

// getRunChildren fetches the shard/child subtree of a run — the runs
// spawned with parent_run_id == runId — via GET /api/runs/{id}/children.
// Returns an empty array for a run with no children (a top-level or
// leaf run), so callers can render a collapsed/absent panel without a
// null guard.
export async function getRunChildren(runId: string): Promise<RunSummary[]> {
  const res = await request<{ runs: RunSummary[] }>(
    `/runs/${encodeURIComponent(runId)}/children`,
  );
  return res.runs ?? [];
}
