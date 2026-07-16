// Pure merge of a pipeline tree's output channels into one "produced
// elements" list for the pipeline board sidebar:
//
//   - artifact files — arbitrary binaries/text a tool wrote into
//     $ITERION_ARTIFACT_FILES_DIR (renders, SBOMs, generated media). These
//     are the designed "outputs" area and are previewable/downloadable.
//   - worktree changes — files added/modified in the run's working
//     directory, from /files?mode=produced (combined git view while the
//     worktree lives, branch-range fallback after finalization). Pure
//     deletions are NOT produced elements and are dropped.
//   - touched files — files the run's LLM nodes wrote/edited, derived from
//     tool_started events (/files/touched). This channel carries the
//     per-node attribution and is the trust anchor for runs that executed
//     in place.
//
// The git channel is only equal to "what the pipeline produced" when the
// run executed in an isolated worktree. For an in-place run (worktree:
// none, non-git workspace) `git status` also reports the operator's own
// pre-existing dirty files — so for those runs the git rows are gated on
// the touched set (a git row nobody's node wrote is ambient workspace
// state, not a produced element), and touched-only rows (e.g. files the
// run already committed) are added bare.
//
// Runs sharing one working directory (a parent and its sub-bots in the
// same worktree) are deduped to one row per path, attributed to every
// node that wrote it; the artifact channel stays per-run (artifact areas
// are run-scoped by construction).

import type { ArtifactFile, RunFile, TouchedFile } from "@/api/runs";

import { classifyProducedFile, type ProducedFileKind } from "./fileKind";

export interface ProducedItem {
  // Stable React key, unique across both channels and every run in the tree.
  key: string;
  // The run in the pipeline tree that produced this element (the root or a
  // sub-bot). Drives preview / download / console-link targeting; for a
  // committed change row it is the run whose branch range contains it.
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

  // change-only. `status` is undefined for a touched-only row (no git
  // counterpart — e.g. an in-place run's already-committed edit); the UI
  // hides the +/- counters for those.
  status?: string;
  added?: number;
  deleted?: number;
  binary?: boolean;
  lifecycle?: "committed" | "uncommitted";
  // Workflow nodes that wrote this file (from the touched channel), in
  // first-write order. Undefined when no attribution is known.
  nodes?: string[];
}

// RunProducedSource is one run's fetched output channels, positionally
// assembled by the component layer. Any field may be undefined while its
// query is loading/errored — a missing channel contributes nothing.
export interface RunProducedSource {
  runId: string;
  // /files?mode=produced
  files?: RunFile[];
  // The /files response's `available` flag: true when the git channel
  // actually answered with a file list. A worktree run whose git view is
  // unavailable (cloud run mid-flight → reason "building", errored query)
  // must fall back to the touched channel rather than trusting an empty
  // git set and showing nothing.
  filesAvailable?: boolean;
  workDir?: string;
  worktree?: boolean;
  // /files/touched
  touched?: TouchedFile[];
  // /artifact-files
  artifacts?: ArtifactFile[];
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

function artifactItems(s: RunProducedSource): ProducedItem[] {
  return (s.artifacts ?? []).map((a) => ({
    key: `artifact:${s.runId}:${a.path}`,
    runId: s.runId,
    path: a.path,
    name: basename(a.path),
    kind: classifyProducedFile(a.path),
    source: "artifact" as const,
    size: a.size,
    modifiedAt: a.modified_at,
  }));
}

interface GitEntry {
  runId: string;
  file: RunFile;
}

interface TouchAcc {
  nodes: string[];
  // First run (tree order) whose nodes wrote the path — the console/diff
  // target for a touched-only row.
  runId: string;
}

interface WorkdirGroup {
  // True when at least one run in the group executed in an isolated
  // worktree: every git row is then the pipeline's own work. False =
  // in-place execution, where git status includes ambient operator state
  // and rows must be gated on the touched set.
  trusted: boolean;
  // True when at least one run's git channel answered (available=true).
  // A trusted group without a delivered git view falls back to touched
  // rows instead of rendering an empty list.
  gitDelivered: boolean;
  git: Map<string, GitEntry[]>;
  touched: Map<string, TouchAcc>;
  // Paths git reported as pure deletions ("D"): dropped from the git
  // rows AND used to suppress touched rows for files that no longer
  // exist — a removed file is not produced.
  deleted: Set<string>;
}

// aggregateProducedItems merges every run in a pipeline tree into one list,
// ordered globally: artifacts first, NEWEST first (across runs — the
// freshest output is the one a pending review is usually about), then
// change rows by path (git reports no per-file mtime).
export function aggregateProducedItems(sources: RunProducedSource[]): ProducedItem[] {
  const all: ProducedItem[] = [];
  for (const s of sources) all.push(...artifactItems(s));

  // Group the change channels by working directory so a parent and its
  // sub-bots sharing one worktree yield one row per path instead of one
  // per run. Runs whose workDir is still unknown (files query loading)
  // group by run id — a transient, self-correcting split.
  const groups = new Map<string, WorkdirGroup>();
  for (const s of sources) {
    const key = s.workDir ? `dir:${s.workDir}` : `run:${s.runId}`;
    let g = groups.get(key);
    if (!g) {
      g = {
        trusted: false,
        gitDelivered: false,
        git: new Map(),
        touched: new Map(),
        deleted: new Set(),
      };
      groups.set(key, g);
    }
    if (s.worktree === true) g.trusted = true;
    if (s.filesAvailable === true) g.gitDelivered = true;
    for (const f of s.files ?? []) {
      if (f.status === "D") {
        g.deleted.add(f.path); // a removed file is not produced
        continue;
      }
      const entries = g.git.get(f.path);
      if (entries) entries.push({ runId: s.runId, file: f });
      else g.git.set(f.path, [{ runId: s.runId, file: f }]);
    }
    for (const t of s.touched ?? []) {
      let acc = g.touched.get(t.path);
      if (!acc) {
        acc = { nodes: [], runId: s.runId };
        g.touched.set(t.path, acc);
      }
      for (const n of t.node_ids ?? []) {
        if (!acc.nodes.includes(n)) acc.nodes.push(n);
      }
    }
  }

  for (const [key, g] of groups) {
    // Trusted (worktree) groups with a delivered git view: git is
    // authoritative — touched only annotates. Everything else (in-place
    // execution, or a worktree whose git channel is unavailable — e.g. a
    // cloud run mid-flight) falls back to the touched set: only paths
    // some node actually wrote qualify, minus paths git reports deleted;
    // git enriches survivors with real +/- counts when present.
    const paths =
      g.trusted && g.gitDelivered
        ? [...g.git.keys()]
        : [...g.touched.keys()].filter((p) => !g.deleted.has(p));
    for (const path of paths) {
      const entries = g.git.get(path) ?? [];
      // The uncommitted entry wins on collision (mirrors the server's
      // combined-mode rule): it reflects the in-flight state, and its
      // owning run's uncommitted diff is valid from any run sharing the
      // directory. A committed row must keep the run whose branch range
      // recorded it, or the diff link would miss.
      const chosen =
        entries.find((e) => e.file.lifecycle !== "committed") ?? entries[0];
      const touch = g.touched.get(path);
      const f = chosen?.file;
      all.push({
        key: `change:${key}:${path}`,
        runId: chosen?.runId ?? touch?.runId ?? "",
        path,
        name: basename(path),
        kind: classifyProducedFile(path),
        source: "change",
        status: f?.status,
        added: f?.added,
        deleted: f?.deleted,
        binary: f?.binary,
        lifecycle: f?.lifecycle,
        nodes: touch && touch.nodes.length > 0 ? touch.nodes : undefined,
      });
    }
  }

  all.sort((a, b) => {
    if (a.source !== b.source) return a.source === "artifact" ? -1 : 1;
    if (a.source === "artifact") return artifactRecency(a, b);
    return a.path.localeCompare(b.path) || a.runId.localeCompare(b.runId);
  });
  return all;
}
