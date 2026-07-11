// Extracted from api/runs.ts to keep that file focused.
// GET endpoints that enumerate runs: full list, distinct repos
// (cloud-mode filter chips), and the cross-store "global active" feed.

import { request } from "./client";
import type {
  GlobalActiveRun,
  ListRunsParams,
  RunRepo,
  RunSummary,
} from "./types";

export async function listRuns(params: ListRunsParams = {}): Promise<RunSummary[]> {
  const qs = new URLSearchParams();
  if (params.status) qs.set("status", params.status);
  if (params.workflow) qs.set("workflow", params.workflow);
  if (params.repo) qs.set("repo", params.repo);
  if (params.since) qs.set("since", params.since);
  if (params.limit) qs.set("limit", String(params.limit));
  if (params.node) qs.set("node", params.node);
  if (params.bot) qs.set("bot", params.bot);
  const suffix = qs.toString();
  const res = await request<{ runs: RunSummary[] }>(
    `/runs${suffix ? `?${suffix}` : ""}`,
  );
  return res.runs ?? [];
}

// listRunRepos fetches the distinct repositories (cloud project_path)
// that have runs in the caller's tenant, with counts — feeds the
// run-list "by repo" filter chips. Cloud-mode only; returns [] in local
// mode (local/manual runs carry no project_path).
export async function listRunRepos(): Promise<RunRepo[]> {
  const res = await request<{ repos: RunRepo[] }>(`/v1/runs/repos`);
  return res.repos ?? [];
}

export async function listGlobalActiveRuns(): Promise<GlobalActiveRun[]> {
  const res = await request<{ runs: GlobalActiveRun[] }>(`/runs/global-active`);
  return res.runs ?? [];
}
