// Extracted from api/runs.ts to keep that file focused.
// Modified-files panel — git status + diff for the run's working dir,
// plus the live-worktree file editor read/write endpoints.

import { apiURL, request } from "./client";
import type {
  RunFile,
  RunFileContent,
  RunFileDiff,
  RunFiles,
  RunFilesMode,
  RunHeader,
  RunTouchedFiles,
} from "./types";

export async function listRunFiles(
  runId: string,
  opts: { mode?: RunFilesMode } = {},
): Promise<RunFiles> {
  const qs = new URLSearchParams();
  if (opts.mode) qs.set("mode", opts.mode);
  const suffix = qs.toString();
  return request(
    `/runs/${encodeURIComponent(runId)}/files${suffix ? `?${suffix}` : ""}`,
  );
}

// listTouchedFiles returns the files the run's LLM nodes wrote/edited,
// with per-node attribution — derived from run events, not git. The
// pipeline board uses it to scope (in-place runs) and annotate (all runs)
// the produced-elements list.
export async function listTouchedFiles(runId: string): Promise<RunTouchedFiles> {
  return request(`/runs/${encodeURIComponent(runId)}/files/touched`);
}

export async function getRunFileDiff(
  runId: string,
  path: string,
  opts: { mode?: RunFilesMode } = {},
): Promise<RunFileDiff> {
  const qs = new URLSearchParams({ path });
  if (opts.mode) qs.set("mode", opts.mode);
  return request(
    `/runs/${encodeURIComponent(runId)}/files/diff?${qs.toString()}`,
  );
}

// getRunFileContent reads one worktree file for in-run editing. Unlike
// getRunFileDiff (changed files only), this reaches any path under the
// worktree — including an unchanged/untracked `.gitignore`. 409 when the
// run has no live worktree (finalized/gc'd); the caller should only offer
// editing while the worktree exists.
export async function getRunFileContent(
  runId: string,
  path: string,
): Promise<RunFileContent> {
  const qs = new URLSearchParams({ path });
  return request(
    `/runs/${encodeURIComponent(runId)}/files/content?${qs.toString()}`,
  );
}

// saveRunFileContent writes operator-edited content back into the run's live
// worktree. Path-traversal is enforced server-side (never escapes work_dir).
export async function saveRunFileContent(
  runId: string,
  path: string,
  content: string,
): Promise<RunFileContent> {
  return request(`/runs/${encodeURIComponent(runId)}/files/content`, {
    method: "PUT",
    body: JSON.stringify({ path, content }),
  });
}

// mergeActionReady reports whether the run has reached the phase where the
// "Squash & merge" action is shown in the Commits panel: a terminal state
// (finished or cancelled — RecoverFinalize populates final_branch for both)
// with a persistent storage branch to merge. It is the single signal that
// flips the FilesPanel's smart default from "combined" (in-flight) to
// "branch" (the committed diff that would actually merge). Mirrors
// CommitsPanel's `mergeable && hasBranch` gate so the two stay in lock-step.
export function mergeActionReady(
  run: Pick<RunHeader, "status" | "final_branch"> | null | undefined,
): boolean {
  if (!run) return false;
  const terminal = run.status === "finished" || run.status === "cancelled";
  return terminal && Boolean(run.final_branch);
}


// ---------------------------------------------------------------------------
// Review scope — what a human gate is asking the operator to approve
// ---------------------------------------------------------------------------

// A gate's range is everything the run changed since the PREVIOUS gate, and
// the groups partition it rather than filtering it: work by node kinds that
// record no boundary (subbots, fan-out branches, computes) lands in the
// trailing group with an empty node_id. A reviewer must never be shown less
// than what changed.
export interface ReviewScopeGroup {
  node_id: string;
  label: string;
  iteration?: number;
  files: RunFile[];
}

export interface ReviewScope {
  run_id: string;
  gate_seq: number;
  base_ref: string;
  head_ref: string;
  available: boolean;
  // Populated whenever available is false, in the operator's terms — a panel
  // that shows nothing without saying why is worse than no panel.
  reason?: string;
  groups: ReviewScopeGroup[];
  total_files: number;
}

// getReviewScope returns the change range of a run's latest human gate, or of
// a specific one when `gate` is given.
export async function getReviewScope(
  runId: string,
  opts: { gate?: number } = {},
): Promise<ReviewScope> {
  const qs = new URLSearchParams();
  if (opts.gate !== undefined) qs.set("gate", String(opts.gate));
  const suffix = qs.toString();
  return request(
    `/runs/${encodeURIComponent(runId)}/review/scope${suffix ? `?${suffix}` : ""}`,
  );
}

// getReviewFileDiff returns one file's before/after within a gate's range.
// The refs are resolved server-side from the gate number; they are never sent
// by the client.
export async function getReviewFileDiff(
  runId: string,
  path: string,
  opts: { gate?: number } = {},
): Promise<RunFileDiff> {
  const qs = new URLSearchParams({ path });
  if (opts.gate !== undefined) qs.set("gate", String(opts.gate));
  return request(
    `/runs/${encodeURIComponent(runId)}/review/diff?${qs.toString()}`,
  );
}

// ---------------------------------------------------------------------------
// Node changes — "an iterion node is like a commit"
// ---------------------------------------------------------------------------

export interface NodeFileChange {
  path: string;
  status: string;
  added?: number;
  deleted?: number;
  binary?: boolean;
}

export interface NodeChangeSet {
  run_id: string;
  node_id: string;
  iteration: number;
  // Which backend answered: "git" for a worktree run, "workspace" for an
  // in-place one.
  source?: string;
  // available:false and an empty file list mean DIFFERENT things — the
  // first is "we cannot tell", the second is "this node changed nothing".
  // reason is always populated for the first.
  available: boolean;
  reason?: string;
  files: NodeFileChange[];
  // Paths a boundary deliberately did not store (oversized). Their content
  // is unavailable; showing them is what stops the panel implying coverage
  // it does not have.
  uncaptured?: string[];
}

// getNodeChanges returns what one node execution did to the workspace.
// `iteration` is the node's loop_iteration — NOT the 0-based index of the
// iteration pills, which is a different number on any looped node.
export async function getNodeChanges(
  runId: string,
  nodeId: string,
  opts: { iteration?: number } = {},
): Promise<NodeChangeSet> {
  const qs = new URLSearchParams();
  if (opts.iteration !== undefined) qs.set("iteration", String(opts.iteration));
  const suffix = qs.toString();
  return request(
    `/runs/${encodeURIComponent(runId)}/nodes/${encodeURIComponent(nodeId)}/changes${suffix ? `?${suffix}` : ""}`,
  );
}

// getNodeFileDiff returns one file's before/after within a node's boundary.
export async function getNodeFileDiff(
  runId: string,
  nodeId: string,
  path: string,
  opts: { iteration?: number } = {},
): Promise<RunFileDiff> {
  const qs = new URLSearchParams({ path });
  if (opts.iteration !== undefined) qs.set("iteration", String(opts.iteration));
  return request(
    `/runs/${encodeURIComponent(runId)}/nodes/${encodeURIComponent(nodeId)}/diff?${qs.toString()}`,
  );
}

// workspaceFileURL streams a path from the run's live workspace (or the
// review-gate head snapshot as fallback). Used by the review panel's
// audio / video / image players — /review/diff is text-oriented and
// refuses multi-MiB binaries.
export function workspaceFileURL(
  runId: string,
  relPath: string,
  opts: { gate?: number; download?: boolean } = {},
): string {
  const segments = relPath.split("/").map(encodeURIComponent).join("/");
  const qs = new URLSearchParams();
  if (opts.gate !== undefined) qs.set("gate", String(opts.gate));
  if (opts.download) qs.set("download", "1");
  const suffix = qs.toString();
  return apiURL(
    `/runs/${encodeURIComponent(runId)}/workspace-files/${segments}${suffix ? `?${suffix}` : ""}`,
  );
}
