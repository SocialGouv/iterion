import type { RunStatus, RunSummary } from "@/api/runs";
import { formatDayHeader } from "@/lib/format";

import { labelForStatus } from "./runStatusMeta";
import {
  metaForSource,
  normalizeSourceKind,
} from "./runSourceMeta";
import {
  repoAxisLabel,
  repoKey,
  repoLabel,
  type RunMode,
} from "./runRepoMeta";

// Available client-side sort axes. The server hard-sorts created_at
// DESC; this is purely a re-arrangement on top of that.
export type SortKey =
  | "started" // created_at DESC (default — preserves the existing behaviour)
  | "updated" // updated_at DESC ("last update")
  | "duration" // wall-clock spent (finished_at||now - created_at) DESC
  | "status" // status alpha ASC, tiebreak by created_at DESC
  | "workflow"; // bundle_name||workflow_name ASC, tiebreak by created_at DESC

// Group-by axes. "none" keeps today's flat list; the others wrap rows
// in labelled section headers. "repo" buckets by the repo/folder axis
// (mode-dependent label — see groupOptionsFor).
export type GroupKey = "none" | "source" | "workflow" | "repo" | "day";

export const SORT_OPTIONS: ReadonlyArray<{ value: SortKey; label: string }> = [
  { value: "started", label: "Started" },
  { value: "updated", label: "Last update" },
  { value: "duration", label: "Duration" },
  { value: "status", label: "Status" },
  { value: "workflow", label: "Workflow" },
];

export const GROUP_OPTIONS: ReadonlyArray<{ value: GroupKey; label: string }> = [
  { value: "none", label: "None" },
  { value: "source", label: "Source" },
  { value: "workflow", label: "Workflow" },
  { value: "repo", label: repoAxisLabel("cloud") }, // "Repo"
  { value: "day", label: "Day" },
];

// Local-mode variant: the "repo" entry reads "Folder" instead of "Repo",
// matching the filter-strip header. Precomputed (not rebuilt per render).
const GROUP_OPTIONS_LOCAL: ReadonlyArray<{ value: GroupKey; label: string }> =
  GROUP_OPTIONS.map((opt) =>
    opt.value === "repo" ? { value: opt.value, label: repoAxisLabel("local") } : opt,
  );

// groupOptionsFor returns the mode-appropriate group dropdown options:
// the "repo" entry is "Repo" in cloud mode, "Folder" locally. Both
// arrays are module constants — stable identity across renders.
export function groupOptionsFor(
  mode: RunMode,
): ReadonlyArray<{ value: GroupKey; label: string }> {
  return mode === "cloud" ? GROUP_OPTIONS : GROUP_OPTIONS_LOCAL;
}

// parseSort/parseGroup are the URL guards. Any unknown value collapses
// to the default so a stale bookmark can't break the page.
export function parseSort(raw: string | null | undefined): SortKey {
  switch (raw) {
    case "started":
    case "updated":
    case "duration":
    case "status":
    case "workflow":
      return raw;
    default:
      return "started";
  }
}

export function parseGroup(raw: string | null | undefined): GroupKey {
  switch (raw) {
    case "source":
    case "workflow":
    case "repo":
    case "day":
      return raw;
    default:
      return "none";
  }
}

// runDurationMs returns the wall-clock the run has consumed since
// created_at. In-flight runs (no finished_at) accrue against `now` so
// the sort stays live while the 1Hz tick re-renders the list.
export function runDurationMs(run: RunSummary, now: number): number {
  const start = Date.parse(run.created_at);
  if (Number.isNaN(start)) return 0;
  const endStr = run.finished_at;
  const end = endStr ? Date.parse(endStr) : now;
  if (Number.isNaN(end)) return 0;
  return Math.max(0, end - start);
}

// workflowLabel mirrors the table's "Workflow" column — bundle_name
// when present, else workflow_name. Used both for sorting and for the
// "Workflow" group key.
export function workflowLabel(run: RunSummary): string {
  return run.bundle_name || run.workflow_name || "";
}

// sortRuns returns a new array sorted by the requested key. Stable on
// tie (Array.prototype.sort is spec-stable since ES2019). `now` is
// injected so tests can pin the in-flight duration deterministically.
export function sortRuns(runs: RunSummary[], key: SortKey, now: number): RunSummary[] {
  const copy = runs.slice();
  copy.sort((a, b) => compareRuns(a, b, key, now));
  return copy;
}

function compareRuns(
  a: RunSummary,
  b: RunSummary,
  key: SortKey,
  now: number,
): number {
  switch (key) {
    case "started":
      // The list comes in created_at DESC from the server; preserve
      // that here so the sort + every secondary tiebreak agree.
      return parseTime(b.created_at) - parseTime(a.created_at);
    case "updated": {
      const cmp = parseTime(b.updated_at) - parseTime(a.updated_at);
      return cmp !== 0 ? cmp : parseTime(b.created_at) - parseTime(a.created_at);
    }
    case "duration": {
      const cmp = runDurationMs(b, now) - runDurationMs(a, now);
      return cmp !== 0 ? cmp : parseTime(b.created_at) - parseTime(a.created_at);
    }
    case "status": {
      const cmp = a.status.localeCompare(b.status);
      return cmp !== 0 ? cmp : parseTime(b.created_at) - parseTime(a.created_at);
    }
    case "workflow": {
      const cmp = workflowLabel(a).localeCompare(workflowLabel(b), undefined, {
        sensitivity: "base",
      });
      return cmp !== 0 ? cmp : parseTime(b.created_at) - parseTime(a.created_at);
    }
  }
}

function parseTime(s: string | undefined): number {
  if (!s) return 0;
  const t = Date.parse(s);
  return Number.isNaN(t) ? 0 : t;
}

// RunGroup is one section to render under a header. The order of the
// returned slice IS the visual order: groupRuns preserves the order
// the first row of each group was discovered, so it inherits whatever
// the prior sort produced.
export interface RunGroup {
  // Stable identifier for React keys and URL anchors — derived from
  // the key plus the discriminator (source kind, workflow label, day
  // bucket). Empty group is impossible: groups only exist when at
  // least one row landed in them.
  id: string;
  // Human-readable header label rendered above the group's rows.
  label: string;
  runs: RunSummary[];
}

// groupRuns slices a (sorted) list into RunGroup sections. The "none"
// key returns a single anonymous group so the caller can iterate
// uniformly; it's the responsibility of the renderer to omit the
// header when groupKey === "none" (signalled by groups.length === 1
// AND key === "none").
export function groupRuns(runs: RunSummary[], key: GroupKey): RunGroup[] {
  if (key === "none") {
    return runs.length === 0 ? [] : [{ id: "all", label: "All runs", runs }];
  }
  const buckets = new Map<string, RunGroup>();
  for (const r of runs) {
    const id = bucketIDFor(r, key);
    const existing = buckets.get(id);
    if (existing) {
      existing.runs.push(r);
      continue;
    }
    buckets.set(id, {
      id,
      label: bucketLabelFor(r, key),
      runs: [r],
    });
  }
  return Array.from(buckets.values());
}

function bucketIDFor(run: RunSummary, key: GroupKey): string {
  switch (key) {
    case "source":
      return `source:${normalizeSourceKind(run.source_kind)}`;
    case "workflow":
      return `workflow:${workflowLabel(run) || "(unnamed)"}`;
    case "repo":
      return `repo:${repoKey(run) || "(none)"}`;
    case "day":
      return `day:${dayBucket(run.created_at)}`;
    case "none":
      return "all";
  }
}

function bucketLabelFor(run: RunSummary, key: GroupKey): string {
  switch (key) {
    case "source":
      return metaForSource(normalizeSourceKind(run.source_kind)).label;
    case "workflow":
      return workflowLabel(run) || "(unnamed workflow)";
    case "repo":
      return repoLabel(run) || "(none)";
    case "day":
      return dayBucketLabel(run.created_at);
    case "none":
      return "All runs";
  }
}

// dayBucket returns a stable YYYY-MM-DD key in the user's local
// timezone — so a run that started "today at 1am" stays in today's
// bucket. Falls through to "(unknown)" for malformed timestamps.
function dayBucket(iso: string | undefined): string {
  if (!iso) return "(unknown)";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "(unknown)";
  const d = new Date(t);
  const y = d.getFullYear();
  const m = `${d.getMonth() + 1}`.padStart(2, "0");
  const day = `${d.getDate()}`.padStart(2, "0");
  return `${y}-${m}-${day}`;
}

// dayBucketLabel formats a day bucket for the group header. Today /
// Yesterday get friendly labels; older days fall back to the shared
// weekday + date form (formatDayHeader).
function dayBucketLabel(iso: string | undefined): string {
  if (!iso) return "(unknown date)";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "(unknown date)";
  const d = new Date(t);
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const startOfRunDay = new Date(d);
  startOfRunDay.setHours(0, 0, 0, 0);
  const diffDays = Math.round(
    (today.getTime() - startOfRunDay.getTime()) / (24 * 60 * 60 * 1000),
  );
  if (diffDays === 0) return "Today";
  if (diffDays === 1) return "Yesterday";
  return formatDayHeader(iso);
}

// statusGroupLabel returns the chip-style label for the status sort —
// used when the operator picks "Status" as the sort axis and wants a
// secondary header per status block. (Not yet wired as a Group axis;
// kept local to this module so future Group("Status") additions stay
// consistent with the chip labels.)
export function statusGroupLabel(status: RunStatus): string {
  return labelForStatus(status);
}
