import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { getNodeChanges, type NodeFileChange, type RunFile } from "@/api/runs";
import { InlineBanner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";

import FileDiffDialog from "../FileDiffDialog";

interface Props {
  runId: string;
  nodeId: string;
  /**
   * The node's loop_iteration — NOT the 0-based index of the iteration
   * pills. The boundary refs are named `…/nodes/<node>/<loopIter>`, so
   * passing the pill index silently resolves a different range: a review
   * loop's iteration 3 sits at index 2, and the panel would show a
   * plausible but wrong diff.
   */
  iteration: number;
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
 * ChangesTab shows the files one node execution changed — the node
 * treated as a commit, bracketed by the boundaries the engine records
 * before it starts and after it finishes.
 *
 * The query lives HERE rather than in NodeDetailPanel on purpose: Radix
 * unmounts an inactive tab panel, so a diff request — far more expensive
 * than the artifact index the parent fetches eagerly — only fires when the
 * operator opens this tab. Its key is (runId, nodeId, iteration), never
 * the exec object, whose identity changes on every WebSocket event and
 * would refire this several times a second on a live run.
 */
export function ChangesTab({ runId, nodeId, iteration }: Props) {
  const [diffFile, setDiffFile] = useState<RunFile | null>(null);

  const { data, isLoading, error } = useQuery({
    queryKey: ["node-changes", runId, nodeId, iteration],
    queryFn: () => getNodeChanges(runId, nodeId, { iteration }),
    retry: false,
  });

  if (isLoading) {
    return <p className="text-caption text-fg-subtle">Loading this node's changes…</p>;
  }
  if (error) {
    return (
      <InlineBanner tone="warning" layout="inline">
        Could not read this node's changes: {errorMessage(error)}
      </InlineBanner>
    );
  }
  // "We cannot tell" and "it changed nothing" are different answers and
  // must not look alike — a subbot or a fan-out branch records no closing
  // boundary, and rendering that as "No changes" would be a lie about the
  // node kind most likely to have rewritten the tree.
  if (!data?.available) {
    return (
      <InlineBanner tone="info" layout="inline">
        {data?.reason ?? "No file changes recorded for this node."}
      </InlineBanner>
    );
  }

  const uncaptured = data.uncaptured ?? [];
  if (data.files.length === 0 && uncaptured.length === 0) {
    return (
      <p className="text-caption text-fg-subtle">
        This node changed no files.
      </p>
    );
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-caption text-fg-subtle">
          {data.files.length} file{data.files.length === 1 ? "" : "s"} changed
          {data.iteration > 0 ? ` · iteration ${data.iteration}` : ""}
        </span>
        {data.source === "workspace" && (
          <span className="text-caption text-fg-subtle" title="Tracked by iterion's own workspace versioning (this run has no git worktree)">
            workspace versioning
          </span>
        )}
      </div>

      <ul className="flex flex-col gap-0.5">
        {data.files.map((f: NodeFileChange) => (
          <li key={f.path}>
            <button
              type="button"
              onClick={() =>
                setDiffFile({
                  path: f.path,
                  status: f.status,
                  added: f.added ?? 0,
                  deleted: f.deleted ?? 0,
                  binary: f.binary,
                })
              }
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
                (f.added || f.deleted) && (
                  <span className="shrink-0 text-fg-subtle">
                    +{f.added ?? 0} −{f.deleted ?? 0}
                  </span>
                )
              )}
            </button>
          </li>
        ))}
      </ul>

      {uncaptured.length > 0 && (
        // Never silent: a file too large to version is not an unchanged
        // file, and on a media pipeline it is usually the deliverable.
        <InlineBanner tone="info" layout="inline">
          {uncaptured.length} file{uncaptured.length === 1 ? " was" : "s were"} too large to
          version, so {uncaptured.length === 1 ? "its" : "their"} diff is unavailable:{" "}
          <span className="font-mono">{uncaptured.slice(0, 3).join(", ")}</span>
          {uncaptured.length > 3 ? ` and ${uncaptured.length - 3} more` : ""}
        </InlineBanner>
      )}

      <FileDiffDialog
        runId={runId}
        file={diffFile}
        nodeId={nodeId}
        nodeIteration={data.iteration}
        onClose={() => setDiffFile(null)}
      />
    </div>
  );
}
