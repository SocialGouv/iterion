import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { getReviewScope, type RunFile } from "@/api/runs";
import FileDiffDialog from "@/components/Runs/FileDiffDialog";
import { InlineBanner } from "@/components/ui";

const POLL_INTERVAL_MS = 5000;

interface Props {
  runId: string;
  /** Poll while the run is still live so late writes appear. */
  live?: boolean;
}

const STATUS_LABEL: Record<string, string> = {
  A: "added",
  M: "modified",
  D: "deleted",
  R: "renamed",
};

function statusTone(status: string): string {
  if (status.startsWith("A")) return "text-success-text";
  if (status.startsWith("D")) return "text-danger-text";
  if (status.startsWith("R")) return "text-accent-text";
  return "text-fg-subtle";
}

/**
 * ReviewScopePanel shows what a human gate is asking the operator to approve:
 * every file the run changed since the PREVIOUS gate, grouped by the node that
 * changed it.
 *
 * The grouping is presentation; the range is the contract. Node groups come
 * from per-node boundary refs, which only main-path nodes record — a subbot, a
 * fan-out branch or a compute node has none, and their work arrives in the
 * trailing group with an empty node_id. It is rendered exactly like the others
 * rather than hidden, because a reviewer approving this must never be shown
 * less than what changed.
 */
export function ReviewScopePanel({ runId, live }: Props) {
  const [diffFile, setDiffFile] = useState<RunFile | null>(null);

  const { data, isLoading, error } = useQuery({
    queryKey: ["review-scope", runId],
    queryFn: () => getReviewScope(runId),
    refetchInterval: live ? POLL_INTERVAL_MS : (false as const),
    refetchIntervalInBackground: false,
    retry: false,
  });

  if (isLoading) {
    return <p className="text-caption text-fg-subtle">Loading the change under review…</p>;
  }
  if (error) {
    return (
      <InlineBanner tone="warning" layout="inline">
        Could not read the change under review: {String(error)}
      </InlineBanner>
    );
  }
  if (!data?.available) {
    // Never a silent empty panel — the reason is the useful part.
    return (
      <InlineBanner tone="info" layout="inline">
        No file diff for this review: {data?.reason ?? "unavailable"}
      </InlineBanner>
    );
  }
  if (data.total_files === 0) {
    return (
      <p className="text-caption text-fg-subtle">
        Nothing changed in the workspace since the previous review.
      </p>
    );
  }

  return (
    <section className="flex flex-col gap-2">
      <header className="flex items-baseline justify-between gap-2">
        <h4 className="text-caption font-medium text-fg">
          Changed since the previous review
        </h4>
        <span className="text-caption text-fg-subtle">
          {data.total_files} file{data.total_files === 1 ? "" : "s"}
        </span>
      </header>

      {data.groups.map((group, i) => (
        <div key={`${group.node_id}-${group.iteration ?? 0}-${i}`} className="flex flex-col gap-1">
          <div className="flex items-center gap-1 text-caption text-fg-subtle">
            {group.node_id ? (
              <code className="text-accent-text" title={group.node_id}>
                {group.node_id}
              </code>
            ) : (
              <span className="italic">{group.label}</span>
            )}
            {group.iteration ? <span>· iteration {group.iteration}</span> : null}
            <span>· {group.files.length}</span>
          </div>
          <ul className="flex flex-col gap-0.5">
            {group.files.map((f: RunFile) => (
              <li key={f.path}>
                <button
                  type="button"
                  onClick={() => setDiffFile(f)}
                  className="flex w-full min-w-0 items-baseline gap-2 rounded px-1 py-0.5 text-left text-caption hover:bg-surface-hover"
                  title={`View the diff of ${f.path}`}
                >
                  <span className={`w-16 shrink-0 ${statusTone(f.status)}`}>
                    {STATUS_LABEL[f.status] ?? f.status}
                  </span>
                  <span className="truncate font-mono text-fg">{f.path}</span>
                  {f.binary ? (
                    <span className="shrink-0 text-fg-subtle">(binary)</span>
                  ) : (
                    <span className="shrink-0 text-fg-subtle">
                      +{f.added} −{f.deleted}
                    </span>
                  )}
                </button>
              </li>
            ))}
          </ul>
        </div>
      ))}

      <FileDiffDialog
        runId={runId}
        file={diffFile}
        // The gate range, not the run range: the reviewer must see the file as
        // they are being asked to approve it, not as later nodes left it.
        gate={data.gate_seq}
        onClose={() => setDiffFile(null)}
      />
    </section>
  );
}
