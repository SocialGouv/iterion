// Extracted from api/runs.ts to keep that file focused.
// GET endpoints that enumerate runs: full list, distinct repos
// (cloud-mode filter chips), and the cross-store "global active" feed.

import { beginRowRequest } from "@/lib/lostRuns";
import { noteRunBirths } from "./rowBirths";
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
  const seq = beginRowRequest();
  const res = await request<{ runs: RunSummary[] }>(
    `/runs${suffix ? `?${suffix}` : ""}`,
  );
  return noteRunBirths("", res.runs ?? [], seq);
}

// listRunRepos fetches the distinct repositories (cloud project_path)
// that have runs in the caller's tenant, with counts — feeds the
// run-list "by repo" filter chips. Cloud-mode only; returns [] in local
// mode (local/manual runs carry no project_path).
export async function listRunRepos(): Promise<RunRepo[]> {
  const res = await request<{ repos: RunRepo[] }>(`/v1/runs/repos`);
  return res.repos ?? [];
}

// listGlobalActiveRuns: each row's birth is noted under the store its link
// names — none for a run of this daemon's own store (own_store), which keeps
// the guarantee; a foreign store's row teaches nothing.
export async function listGlobalActiveRuns(): Promise<GlobalActiveRun[]> {
  const seq = beginRowRequest();
  const res = await request<{ runs: GlobalActiveRun[] }>(`/runs/global-active`);
  const runs = res.runs ?? [];
  for (const run of runs) noteRunBirths(globalRunStoreParam(run), [run], seq);
  return runs;
}

// globalRunStoreParam is the `?store=` a view of a global-active row names on
// this daemon: none for a run of its own store, the run's store otherwise.
export function globalRunStoreParam(run: GlobalActiveRun): string {
  return run.own_store ? "" : (run.store_path ?? "");
}

function sameOrigin(a: string, b: string): boolean {
  try {
    return new URL(a).origin === new URL(b).origin;
  } catch {
    return false;
  }
}

// crossDaemonRunStore is the `?store=` a view of a global-active row names on
// the daemon at daemonURL (the desktop resolved it for the run's store), this
// daemon being at currentURL ("" when unknown): none on another daemon — it
// serves the run's store as its own —, the row's answer on this daemon
// (globalRunStoreParam), and the run's store when the two cannot be told
// apart (the server reads a `?store=` naming its own store as its own).
export function crossDaemonRunStore(run: GlobalActiveRun, daemonURL: string, currentURL: string): string {
  if (!currentURL) return run.store_path ?? "";
  return sameOrigin(daemonURL, currentURL) ? globalRunStoreParam(run) : "";
}
