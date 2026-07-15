// Pure merge of a run's two output channels into one "produced elements"
// list for the pipeline board sidebar:
//
//   - artifact files — arbitrary binaries/text a tool wrote into
//     $ITERION_ARTIFACT_FILES_DIR (renders, SBOMs, generated media). These
//     are the designed "outputs" area and are previewable/downloadable.
//   - worktree changes — files the run added/modified in its git worktree
//     (source code, committed assets), from the combined git-status+branch
//     diff. Pure deletions are NOT produced elements and are dropped.
//
// The two channels live in disjoint path namespaces (artifact-area-relative
// vs worktree-relative), so no cross-channel de-duplication is needed. The
// component layer owns fetching + rendering; this module owns only the shape.

import type { ArtifactFile, RunFile } from "@/api/runs";

import { classifyProducedFile, type ProducedFileKind } from "./fileKind";

export interface ProducedItem {
  // Stable React key, unique across both channels and every run in the tree.
  key: string;
  // The run in the pipeline tree that produced this element (the root or a
  // sub-bot). Drives preview / download / console-link targeting.
  runId: string;
  // Slash-separated display path within its channel.
  path: string;
  // Basename for the primary label.
  name: string;
  kind: ProducedFileKind;
  source: "artifact" | "change";

  // artifact-only
  size?: number;
  modifiedAt?: string;

  // change-only
  status?: string;
  added?: number;
  deleted?: number;
  binary?: boolean;
  lifecycle?: "committed" | "uncommitted";
}

function basename(path: string): string {
  const i = path.lastIndexOf("/");
  return i >= 0 ? path.slice(i + 1) : path;
}

// artifactRecency orders two artifact items newest-first by modified_at (a
// missing/empty timestamp sorts last), tie-breaking on path then run for
// stability. Newest-first because the freshest output is almost always the
// one the operator opens to answer a pending review.
function artifactRecency(a: ProducedItem, b: ProducedItem): number {
  const ta = a.modifiedAt ?? "";
  const tb = b.modifiedAt ?? "";
  if (ta !== tb) return tb.localeCompare(ta); // ISO timestamps: lexicographic = chronological
  return a.path.localeCompare(b.path) || a.runId.localeCompare(b.runId);
}

// mergeProducedItems folds one run's artifact-file manifest and worktree
// change list into an ordered list: artifacts first (the explicit outputs,
// NEWEST first), then worktree additions/modifications sorted by path (git
// reports no per-file mtime). A worktree entry whose status is a pure
// deletion ("D") is excluded — a removed file is not something the pipeline
// produced. `runId` is the owning run so keys stay unique when several runs
// in a tree are merged together.
export function mergeProducedItems(
  files: RunFile[] | undefined,
  artifacts: ArtifactFile[] | undefined,
  runId: string,
): ProducedItem[] {
  const out: ProducedItem[] = [];

  const arts = [...(artifacts ?? [])].sort(
    (a, b) =>
      (b.modified_at ?? "").localeCompare(a.modified_at ?? "") ||
      a.path.localeCompare(b.path),
  );
  for (const a of arts) {
    out.push({
      key: `artifact:${runId}:${a.path}`,
      runId,
      path: a.path,
      name: basename(a.path),
      kind: classifyProducedFile(a.path),
      source: "artifact",
      size: a.size,
      modifiedAt: a.modified_at,
    });
  }

  const changes = (files ?? [])
    .filter((f) => f.status !== "D")
    .sort((a, b) => a.path.localeCompare(b.path));
  for (const f of changes) {
    out.push({
      key: `change:${runId}:${f.path}`,
      runId,
      path: f.path,
      name: basename(f.path),
      kind: classifyProducedFile(f.path),
      source: "change",
      status: f.status,
      added: f.added,
      deleted: f.deleted,
      binary: f.binary,
      lifecycle: f.lifecycle,
    });
  }

  return out;
}

// aggregateProducedItems merges every run in a pipeline tree into one list,
// then orders it globally: artifacts first, NEWEST first (across runs — the
// freshest output is the one a pending review is usually about), then
// worktree changes by path (git reports no per-file mtime). Per-run results
// arrive positionally aligned with `runIds` (index i = runIds[i]); a missing
// slot (still loading / errored) contributes nothing.
export function aggregateProducedItems(
  runIds: string[],
  files: Array<RunFile[] | undefined>,
  artifacts: Array<ArtifactFile[] | undefined>,
): ProducedItem[] {
  const all: ProducedItem[] = [];
  runIds.forEach((runId, i) => {
    all.push(...mergeProducedItems(files[i], artifacts[i], runId));
  });
  all.sort((a, b) => {
    if (a.source !== b.source) return a.source === "artifact" ? -1 : 1;
    if (a.source === "artifact") return artifactRecency(a, b);
    return a.path.localeCompare(b.path) || a.runId.localeCompare(b.runId);
  });
  return all;
}
