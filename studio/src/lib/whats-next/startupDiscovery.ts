// Startup-discovery helpers for the whats-next session: find the run
// to auto-attach to, and hydrate the run store from it. The decision
// LOGIC (live-run pick, workflow-name candidates) is pure and unit-
// tested; the async wrappers compose it with the runs API + run store.

import {
  getRunWithRetry,
  listRuns,
  type RunSummary,
} from "@/api/runs";
import { runStore } from "@/store/run";

import { rememberSessionRunId } from "./sessionStorage";

// A bot id may be spelled hyphenated while its workflow name uses
// underscores — probe both, deduped, hyphen-normalized first.
export function candidateWorkflows(botId: string): string[] {
  const candidates = [botId.replace(/-/g, "_"), botId];
  const seen = new Set<string>();
  const out: string[] = [];
  for (const workflow of candidates) {
    if (seen.has(workflow)) continue;
    seen.add(workflow);
    out.push(workflow);
  }
  return out;
}

// First non-terminal run in the (candidate-ordered) match list, or null.
export function pickLiveRunId(matches: RunSummary[]): string | null {
  const active = matches.find(
    (r) =>
      r.status === "queued" ||
      r.status === "running" ||
      r.status === "paused_waiting_human",
  );
  return active?.id ?? null;
}

// findLiveRunForBot returns the id of the most recent non-terminal
// run for this bot's workflow, or null when nothing live exists.
// In cloud with an active repo, the probe is repo-scoped (server
// matches project_path) so a live session on ANOTHER repo doesn't
// hijack this scope's launcher.
export async function findLiveRunForBot(
  botId: string,
  repoFilter: string | undefined,
): Promise<string | null> {
  const matches: RunSummary[] = [];
  for (const workflow of candidateWorkflows(botId)) {
    const runs = await listRuns({ workflow, limit: 10, repo: repoFilter });
    matches.push(...runs);
  }
  return pickLiveRunId(matches);
}

// attachSessionRun hydrates the (module-default) run store from the
// given run and remembers it for this (bot, scope). Returns false when
// `isCancelled()` reports the caller's effect was torn down after the
// snapshot fetch — the caller then skips its own setRunId. Fetch
// errors propagate to the caller (they decide between discovery-error
// and forget-the-memory).
export async function attachSessionRun(opts: {
  runId: string;
  botId: string;
  scopeKey: string | null;
  signal: AbortSignal;
  isCancelled: () => boolean;
}): Promise<boolean> {
  // Retry-on-404: a run surfaced by findLiveRunForBot (or another
  // tab's launch) may still be mid-flush — run.json can lag the
  // listing by a beat. See getRunWithRetry for the race rationale.
  const snap = await getRunWithRetry(opts.runId, { signal: opts.signal });
  if (opts.isCancelled()) return false;
  // Continuity is the central whats-next promise: when the user
  // returns to /whats-next after a previous session ended, they
  // expect to see the full transcript of that exchange, not a
  // blank launcher offering them to start over.
  runStore.getState().reset();
  runStore.getState().applySnapshot(snap);
  // setRunId on the store FIRST so loadEventHistoryIfMissing's
  // post-await guard (`state.runId !== runId` → return) passes.
  runStore.getState().setRunId(opts.runId);
  try {
    await runStore.getState().loadEventHistoryIfMissing(opts.runId);
  } catch {
    // ignore — the live WS will eventually fill any gap.
  }
  // Remember now so subsequent mounts (within the same origin)
  // skip the discovery query and re-attach via localStorage.
  rememberSessionRunId(opts.botId, opts.scopeKey, opts.runId);
  return true;
}
