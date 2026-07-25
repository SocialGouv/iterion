// Extracted from api/runs.ts to keep that file focused.
// Modified-files panel — git status + diff for the run's working dir,
// plus the live-worktree file editor read/write endpoints.

import { apiURL, request } from "./client";
import type {
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

// runFilePreviewURL serves a passive image from the exact paused run's live
// worktree. Unlike workspace-level image URLs, this also works for isolated
// worktrees whose files have not been merged back into the project.
export function runFilePreviewURL(runId: string, relPath: string): string {
  const segments = relPath.split("/").map(encodeURIComponent).join("/");
  return apiURL(
    `/runs/${encodeURIComponent(runId)}/files/preview/${segments}`,
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
