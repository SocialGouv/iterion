import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { listAllArtifacts, listArtifacts } from "@/api/runs";
import type { RunArtifactSummary } from "@/api/runs/types";
import { Badge, Spinner } from "@/components/ui";
import { humanizeKey } from "@/lib/humanizeKey";
import { useRunStore } from "@/store/run";

import ArtifactDiff from "../ArtifactDiff";
import ArtifactFilesPanel from "../ArtifactFilesPanel";

// ArtifactsPanel is the centralized, label-grouped view of a run's
// artifacts (the "Artifacts" bottom tab). It surfaces every PUBLISHED
// node output (previously buried in the per-node detail panel), grouped by
// label so e.g. all plans sit together — then the tool-written FILES below
// as their own section. Reuses ArtifactDiff for the plan/verdict/diff
// rendering and ArtifactFilesPanel for the files section.
export default function ArtifactsPanel({ runId }: { runId: string }) {
  const artifacts = useRunArtifacts(runId);
  const groups = groupByLabel(artifacts);

  return (
    <div className="h-full min-h-0 overflow-y-auto flex flex-col">
      <section className="flex flex-col gap-3 px-3 py-3">
        <SectionHeading>Published outputs</SectionHeading>
        {artifacts.length === 0 ? (
          <p className="text-caption text-fg-subtle px-1">
            No published artifacts yet. A node's output appears here when it
            declares <code>publish:</code>.
          </p>
        ) : (
          groups.map((g) => (
            <div key={g.label} className="flex flex-col gap-1.5">
              <div className="flex items-center gap-2">
                <Badge variant={g.label === OTHER ? "neutral" : "accent"} size="sm">
                  {g.label}
                </Badge>
                <span className="text-caption text-fg-subtle">{g.items.length}</span>
              </div>
              <div className="flex flex-col gap-1.5">
                {g.items.map((a) => (
                  <ArtifactRow key={`${g.label}:${a.node_id}`} runId={runId} a={a} />
                ))}
              </div>
            </div>
          ))
        )}
      </section>

      <section className="flex flex-col min-h-0 flex-1 border-t border-border-default">
        <div className="px-3 pt-3">
          <SectionHeading>Files</SectionHeading>
        </div>
        <div className="flex-1 min-h-0">
          <ArtifactFilesPanel runId={runId} />
        </div>
      </section>
    </div>
  );
}

function SectionHeading({ children }: { children: React.ReactNode }) {
  return (
    <h3 className="text-caption font-semibold uppercase tracking-wide text-fg-subtle">
      {children}
    </h3>
  );
}

// ArtifactRow is one published-artifact card: a collapsed header (title /
// humanized node id + label chips) that lazy-loads the node's versions and
// renders ArtifactDiff (plan/verdict cards + version diff) on expand.
function ArtifactRow({ runId, a }: { runId: string; a: RunArtifactSummary }) {
  const [open, setOpen] = useState(false);

  // Lazy-loaded on first expand only (staleTime: Infinity mirrors the
  // previous fetch-once-per-row behavior — collapsing and re-expanding
  // doesn't refetch). Best-effort: a load failure just leaves the
  // spinner, so the query error is deliberately unread.
  const versionsQuery = useQuery({
    queryKey: ["run-artifact-versions", runId, a.node_id],
    queryFn: ({ signal }) => listArtifacts(runId, a.node_id, { signal }),
    enabled: open,
    staleTime: Infinity,
  });
  const versions = versionsQuery.data ?? null;

  const title = a.title || humanizeKey(a.node_id);
  return (
    <details
      className="group rounded-lg border border-border-default bg-surface-1"
      onToggle={(e) => setOpen((e.currentTarget as HTMLDetailsElement).open)}
    >
      <summary className="flex cursor-pointer items-center gap-2 px-3 py-2 list-none">
        <span
          className="text-fg-subtle transition-transform group-open:rotate-90 flex-none"
          aria-hidden
        >
          ▸
        </span>
        <span className="text-label font-medium text-fg-default">{title}</span>
        {(a.labels ?? []).map((l) => (
          <Badge key={l} variant="neutral" size="sm">
            {l}
          </Badge>
        ))}
        <span className="ml-auto text-caption text-fg-subtle font-mono">
          v{a.version}
        </span>
      </summary>
      <div className="px-3 pb-3 pt-1 border-t border-border-subtle">
        {!open ? null : versions === null ? (
          <Spinner size="sm" />
        ) : (
          <ArtifactDiff runId={runId} nodeId={a.node_id} versions={versions} />
        )}
      </div>
    </details>
  );
}

const OTHER = "other";

interface LabelGroup {
  label: string;
  items: RunArtifactSummary[];
}

// groupByLabel buckets artifacts by label — a multi-label artifact appears
// in each of its groups; an unlabelled one falls into "other". Known
// labels (plan, verdict) sort first, then the rest alphabetically, with
// "other" always last.
function groupByLabel(artifacts: RunArtifactSummary[]): LabelGroup[] {
  const byLabel = new Map<string, RunArtifactSummary[]>();
  for (const a of artifacts) {
    const labels = a.labels && a.labels.length > 0 ? a.labels : [OTHER];
    for (const l of labels) {
      const arr = byLabel.get(l) ?? [];
      arr.push(a);
      byLabel.set(l, arr);
    }
  }
  const order = (l: string) => {
    if (l === "plan") return 0;
    if (l === "verdict") return 1;
    if (l === OTHER) return 3;
    return 2;
  };
  return Array.from(byLabel.entries())
    .map(([label, items]) => ({ label, items }))
    .sort((x, y) => order(x.label) - order(y.label) || x.label.localeCompare(y.label));
}

// REFRESH_EVENTS are the event types after which a node may have published
// a fresh artifact — the same set ArtifactFilesPanel watches. Refetching
// only on these (not on every event) avoids churn on llm-step/tool events
// for runs that publish nothing.
const REFRESH_EVENTS = new Set([
  "node_finished",
  "run_finished",
  "run_failed",
  "run_cancelled",
]);

// Stable empty fallback so the undefined→loaded transition doesn't hand
// groupByLabel a fresh [] reference each render.
const EMPTY_ARTIFACTS: RunArtifactSummary[] = [];

// useRunArtifacts fetches the aggregate artifact list once per run, then
// invalidates (debounced) when an artifact-producing event arrives.
// Returns [] until loaded; load failures are best-effort (the query error
// is deliberately unread). Mirrors ArtifactFilesPanel's refresh discipline.
function useRunArtifacts(runId: string): RunArtifactSummary[] {
  const queryClient = useQueryClient();
  const events = useRunStore((s) => s.events);
  const lastSeenSeq = useRef<number>(-1);
  const debounce = useRef<ReturnType<typeof setTimeout> | null>(null);

  const query = useQuery({
    queryKey: ["run-artifacts", runId],
    queryFn: ({ signal }) => listAllArtifacts(runId, { signal }),
    enabled: !!runId,
  });

  // Reset the seq high-water mark on run change.
  useEffect(() => {
    lastSeenSeq.current = -1;
  }, [runId]);

  // Debounced refetch when a new artifact-producing event lands.
  useEffect(() => {
    if (!runId || events.length === 0) return;
    let touched = false;
    for (const ev of events) {
      if (ev.seq <= lastSeenSeq.current) continue;
      lastSeenSeq.current = ev.seq;
      if (REFRESH_EVENTS.has(ev.type)) touched = true;
    }
    if (!touched) return;
    if (debounce.current) clearTimeout(debounce.current);
    debounce.current = setTimeout(() => {
      void queryClient.invalidateQueries({ queryKey: ["run-artifacts", runId] });
    }, 300);
    return () => {
      if (debounce.current) clearTimeout(debounce.current);
    };
  }, [events, runId, queryClient]);

  return query.data ?? EMPTY_ARTIFACTS;
}
