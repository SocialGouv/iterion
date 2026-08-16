import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { ChevronRightIcon } from "@radix-ui/react-icons";

import { getReviewScope, workspaceFileURL, type RunFile } from "@/api/runs";
import FileDiffDialog from "@/components/Runs/FileDiffDialog";
import { Dialog, InlineBanner, Spinner } from "@/components/ui";

import {
  classifyProducedFile,
  producedKindLabel,
  type ProducedFileKind,
} from "./fileKind";
import { MediaPreviewDialog, type MediaPreviewKind } from "./ImagePreview";

const POLL_INTERVAL_MS = 5000;

interface Props {
  runId: string;
  /** Poll while the run is still live so late writes appear. */
  live?: boolean;
  /**
   * Identifies WHICH pause is being reviewed — a run reaching a second
   * gate keeps the same runId, so this is what separates the two ranges
   * in the query cache. Callers pass the same value they key the answer
   * form on (interaction id + updated_at).
   */
  pauseKey?: string;
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

function isMediaKind(kind: ProducedFileKind): boolean {
  return kind === "image" || kind === "audio" || kind === "video";
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
 *
 * The list itself sits in a closed-by-default accordion: a media pipeline can
 * produce dozens of files and the form below is the action the operator needs
 * first. Opening a row opens the media preview dialog (audio/video/image)
 * or a text diff.
 */
export function ReviewScopePanel({ runId, live, pauseKey }: Props) {
  const [diffFile, setDiffFile] = useState<RunFile | null>(null);
  const [mediaFile, setMediaFile] = useState<RunFile | null>(null);

  const { data, isLoading, error } = useQuery({
    // `pauseKey` is part of the key, not decoration. A run that reaches a
    // SECOND gate keeps the same runId, and React reconciles this panel in
    // place when the board poll swaps in the new pending review — same
    // element type, same props. Without a distinct key the cached payload
    // is reused, and since polling stops once a range resolves (below),
    // refetchOnWindowFocus is off globally and the pause has no other
    // trigger, it is never refetched: the operator would approve gate N
    // while reading gate N-1's file list. Exactly the multi-gate pipeline
    // this panel exists for.
    queryKey: ["review-scope", runId, pauseKey ?? ""],
    queryFn: () => getReviewScope(runId),
    // Poll only until a range resolves, then stop for good.
    //
    // The panel is shown for a run paused at a gate, and a gate's range is
    // frozen for the lifetime of that pause: both `gate/N-1..gate/N` and
    // the workspacetrack snapshot ids are written once and never move. So
    // every further poll returned a byte-identical payload while costing,
    // server-side, two `git diff` forks per recorded node boundary — ~82
    // git processes every 5s on a 40-boundary run, per open review card,
    // and the board can render several.
    //
    // The initial poll is kept because the panel can mount in the moment
    // between the pause surfacing and the gate ref being readable; with
    // `retry: false` a single cold miss would otherwise leave the operator
    // with a permanently empty panel.
    refetchInterval: (query) =>
      live && !query.state.data?.available ? POLL_INTERVAL_MS : (false as const),
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

  const openFile = (f: RunFile) => {
    if (f.status === "D") {
      // Deleted paths have no live content to play; the text diff still
      // shows the before side when it is textual.
      setDiffFile(f);
      return;
    }
    const kind = classifyProducedFile(f.path);
    if (isMediaKind(kind) || f.binary) {
      // Binary media: play in place. Binary non-media still gets the
      // dialog (download-only fallback). Text/data opens the Monaco diff.
      if (isMediaKind(kind)) {
        setMediaFile(f);
        return;
      }
    }
    setDiffFile(f);
  };

  return (
    <section className="flex flex-col gap-1">
      <details className="group rounded-md border border-border-default bg-surface-1 open:bg-surface-1">
        <summary className="flex cursor-pointer list-none items-center gap-2 px-2 py-1.5 text-caption select-none [&::-webkit-details-marker]:hidden">
          <ChevronRightIcon className="h-3.5 w-3.5 shrink-0 text-fg-subtle transition-transform group-open:rotate-90" />
          <span className="font-medium text-fg">
            Changed since the previous review
          </span>
          <span className="text-fg-subtle">
            {data.total_files} file{data.total_files === 1 ? "" : "s"}
          </span>
          <span className="ml-auto text-micro text-fg-subtle group-open:hidden">
            open to browse
          </span>
        </summary>

        <div className="flex flex-col gap-2 border-t border-border-default px-2 py-2">
          {data.groups.map((group, i) => (
            <div
              key={`${group.node_id}-${group.iteration ?? 0}-${i}`}
              className="flex flex-col gap-1"
            >
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
                {group.files.map((f: RunFile) => {
                  const kind = classifyProducedFile(f.path);
                  const media = isMediaKind(kind);
                  return (
                    <li key={f.path}>
                      <button
                        type="button"
                        // An uncaptured non-media row has no content on
                        // either side, so /review/diff would 500 with
                        // "path not in either snapshot" — an opaque server
                        // error from a row the panel just labelled "(not
                        // versioned)". Media still opens: that dialog
                        // streams the LIVE file, which does exist.
                        disabled={!!f.uncaptured && !media}
                        onClick={() => openFile(f)}
                        className="flex w-full min-w-0 items-baseline gap-2 rounded px-1 py-0.5 text-left text-caption enabled:hover:bg-surface-hover disabled:cursor-default"
                        title={
                          media
                            ? `Play ${producedKindLabel(kind).toLowerCase()}: ${f.path}`
                            : `View the diff of ${f.path}`
                        }
                      >
                        <span className={`w-16 shrink-0 ${statusTone(f.status)}`}>
                          {STATUS_LABEL[f.status] ?? f.status}
                        </span>
                        {media ? (
                          <span className="shrink-0 text-micro uppercase tracking-wide text-accent-text">
                            {kind}
                          </span>
                        ) : null}
                        <span className="truncate font-mono text-fg">{f.path}</span>
                        {f.uncaptured ? (
                          <span
                            className="shrink-0 text-fg-subtle"
                            title="Too large to version — listed so the range stays complete, but no diff can be shown"
                          >
                            (not versioned)
                          </span>
                        ) : f.binary || media ? (
                          <span className="shrink-0 text-fg-subtle">
                            {media ? "play" : "(binary)"}
                          </span>
                        ) : f.counts_unknown ? null : (
                          /* counts_unknown: the workspace backend stores
                             content, not diffs, so it cannot produce line
                             counts — rendering the zeros would read as
                             "nothing changed in this file". */
                          <span className="shrink-0 text-fg-subtle">
                            +{f.added} −{f.deleted}
                          </span>
                        )}
                      </button>
                    </li>
                  );
                })}
              </ul>
            </div>
          ))}
        </div>
      </details>

      <FileDiffDialog
        runId={runId}
        file={diffFile}
        // The gate range, not the run range: the reviewer must see the file as
        // they are being asked to approve it, not as later nodes left it.
        gate={data.gate_seq}
        onClose={() => setDiffFile(null)}
      />

      {mediaFile ? (
        <WorkspaceMediaDialog
          runId={runId}
          file={mediaFile}
          gate={data.gate_seq}
          onClose={() => setMediaFile(null)}
        />
      ) : null}
    </section>
  );
}

// WorkspaceMediaDialog plays / shows a review-scope media file streamed
// from the run workspace (or the gate head snapshot as fallback). Direct
// media element src keeps multi-MiB tracks off the heap (unlike blob
// ObjectURLs).
function WorkspaceMediaDialog({
  runId,
  file,
  gate,
  onClose,
}: {
  runId: string;
  file: RunFile;
  gate: number;
  onClose: () => void;
}) {
  const kind = classifyProducedFile(file.path);
  const src = useMemo(
    () => workspaceFileURL(runId, file.path, { gate }),
    [runId, file.path, gate],
  );
  const downloadHref = useMemo(
    () => workspaceFileURL(runId, file.path, { gate, download: true }),
    [runId, file.path, gate],
  );

  const description = (
    <span>
      {STATUS_LABEL[file.status] ?? file.status}
      {file.binary ? " · media" : ""}
    </span>
  );

  if (isMediaKind(kind)) {
    return (
      <MediaPreviewDialog
        kind={kind as MediaPreviewKind}
        open
        onOpenChange={(open) => {
          if (!open) onClose();
        }}
        src={src}
        alt={file.path}
        title={<span className="font-mono text-xs">{file.path}</span>}
        description={description}
        downloadHref={downloadHref}
      />
    );
  }

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      widthClass="max-w-3xl"
      title={<span className="font-mono text-xs">{file.path}</span>}
      description={description}
    >
      <div className="flex h-32 items-center justify-center gap-2 text-xs text-fg-subtle">
        <Spinner /> Loading…
      </div>
    </Dialog>
  );
}
