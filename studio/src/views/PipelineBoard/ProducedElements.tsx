import { useState } from "react";
import { useQueries } from "@tanstack/react-query";
import { Link } from "wouter";

import {
  ArchiveIcon,
  CodeIcon,
  DownloadIcon,
  ExternalLinkIcon,
  FileIcon,
  FileTextIcon,
  ImageIcon,
  ReloadIcon,
  SpeakerLoudIcon,
  TableIcon,
  VideoIcon,
} from "@radix-ui/react-icons";

import {
  artifactFileURL,
  listArtifactFiles,
  listRunFiles,
} from "@/api/runs";
import { Badge, Button, Dialog, EmptyState, IconButton, Spinner } from "@/components/ui";
import { usePreview, type PreviewState } from "@/components/Runs/usePreview";
import { formatBytes, formatRelative } from "@/lib/format";

import { producedKindLabel, type ProducedFileKind } from "./fileKind";
import { aggregateProducedItems, type ProducedItem } from "./producedItems";

// Statuses past which the run's outputs no longer change — polling stops.
const TERMINAL = new Set(["finished", "failed", "cancelled"]);
const POLL_INTERVAL_MS = 3000;
// Fan-out cap: each run in the tree costs two polled requests, so bound how
// many we query at once. The root is always first (walk order), so slicing
// keeps it; the overflow is surfaced, never silently dropped.
const MAX_TREE_RUNS = 30;

interface Props {
  // The pipeline's whole run tree — root first, then descendants. Outputs are
  // fetched for every run so a sub-bot's produced elements surface here too.
  runIds: string[];
  // Root run status; drives whether we keep polling for new outputs.
  status?: string;
}

// ProducedElements lists everything a pipeline (root + sub-bots) has produced
// so far — the files written into each run's artifact area (previewable/
// downloadable) plus the source files it added/modified in its worktree
// (linked to that run's console). It polls while the pipeline is live so new
// outputs appear as they land.
export function ProducedElements({ runIds, status }: Props) {
  const polling = !status || !TERMINAL.has(status);
  const effectiveRunIds = runIds.slice(0, MAX_TREE_RUNS);
  const truncatedRuns = runIds.length - effectiveRunIds.length;
  const multiRun = effectiveRunIds.length > 1;

  const fileQueries = useQueries({
    queries: effectiveRunIds.map((id) => ({
      queryKey: ["pipeline-produced-files", id],
      queryFn: () => listRunFiles(id, { mode: "combined" as const }),
      refetchInterval: polling ? POLL_INTERVAL_MS : (false as const),
      refetchIntervalInBackground: false,
      retry: false,
    })),
  });
  const artifactQueries = useQueries({
    queries: effectiveRunIds.map((id) => ({
      queryKey: ["pipeline-produced-artifacts", id],
      queryFn: () => listArtifactFiles(id),
      refetchInterval: polling ? POLL_INTERVAL_MS : (false as const),
      refetchIntervalInBackground: false,
      retry: false,
    })),
  });

  const items = aggregateProducedItems(
    effectiveRunIds,
    fileQueries.map((q) => q.data?.files),
    artifactQueries.map((q) => q.data),
  );

  const { preview, openPreview, closePreview } = usePreview(effectiveRunIds[0] ?? null);
  // The run the open preview belongs to — the download link must target it,
  // not the root.
  const [previewRunId, setPreviewRunId] = useState("");
  const handlePreview = (item: ProducedItem) => {
    setPreviewRunId(item.runId);
    openPreview({ path: item.path, size: item.size ?? 0 }, item.runId);
  };

  const loading =
    fileQueries.some((q) => q.isLoading) || artifactQueries.some((q) => q.isLoading);
  const fetching =
    fileQueries.some((q) => q.isFetching) || artifactQueries.some((q) => q.isFetching);
  const building = fileQueries.some((q) => q.data?.reason === "building");
  const refresh = () => {
    fileQueries.forEach((q) => void q.refetch());
    artifactQueries.forEach((q) => void q.refetch());
  };

  return (
    <section aria-label="Produced elements" className="space-y-2">
      <div className="flex items-center gap-2">
        <h3 className="text-xs font-semibold text-fg-default">Produced elements</h3>
        {items.length > 0 && <Badge variant="neutral">{items.length}</Badge>}
        {multiRun && (
          <span className="text-micro text-fg-subtle">
            across {effectiveRunIds.length} runs
          </span>
        )}
        <div className="ml-auto">
          <IconButton
            label="Refresh produced elements"
            size="sm"
            variant="ghost"
            onClick={refresh}
            disabled={fetching}
          >
            <ReloadIcon className={fetching ? "animate-spin" : undefined} />
          </IconButton>
        </div>
      </div>

      {truncatedRuns > 0 && (
        <p className="text-micro text-warning-fg">
          Showing outputs for the first {MAX_TREE_RUNS} of {runIds.length} runs in
          this pipeline; {truncatedRuns} more are not shown.
        </p>
      )}

      {loading && items.length === 0 ? (
        <div className="flex items-center gap-2 py-4 text-xs text-fg-muted">
          <Spinner /> Loading produced elements…
        </div>
      ) : items.length === 0 ? (
        <EmptyState
          title="Nothing produced yet"
          message={
            building
              ? "Outputs appear here as the pipeline writes files and commits changes."
              : "This pipeline hasn't written any output files or changed any code yet."
          }
        />
      ) : (
        <ul className="space-y-1">
          {items.map((item) => (
            <ProducedRow
              key={item.key}
              item={item}
              multiRun={multiRun}
              onPreview={() => handlePreview(item)}
            />
          ))}
        </ul>
      )}

      {preview && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) closePreview();
          }}
          widthClass="max-w-3xl"
          title={<span className="font-mono text-xs">{preview.path}</span>}
          description={
            <span>
              {formatBytes(preview.size)} · {preview.contentType || "loading…"}
            </span>
          }
          footer={
            <a
              href={`${artifactFileURL(previewRunId, preview.path)}?download=1`}
              download
              className="inline-flex items-center gap-1 rounded-md border border-border-default px-2.5 py-1 text-xs font-medium text-fg-default hover:bg-surface-2"
            >
              <DownloadIcon /> Download
            </a>
          }
        >
          <PreviewBody preview={preview} />
        </Dialog>
      )}
    </section>
  );
}

// KIND_ICON maps a produced kind to its row glyph. Media kinds (image/audio/
// video) get a distinct icon so the operator can eyeball an output list of
// mixed types at a glance.
function KindIcon({ kind }: { kind: ProducedFileKind }) {
  const cls = "h-3.5 w-3.5 shrink-0 text-fg-muted";
  switch (kind) {
    case "code":
      return <CodeIcon className={cls} aria-hidden />;
    case "image":
      return <ImageIcon className={cls} aria-hidden />;
    case "audio":
      return <SpeakerLoudIcon className={cls} aria-hidden />;
    case "video":
      return <VideoIcon className={cls} aria-hidden />;
    case "doc":
      return <FileTextIcon className={cls} aria-hidden />;
    case "data":
      return <TableIcon className={cls} aria-hidden />;
    case "archive":
      return <ArchiveIcon className={cls} aria-hidden />;
    default:
      return <FileIcon className={cls} aria-hidden />;
  }
}

function ProducedRow({
  item,
  multiRun,
  onPreview,
}: {
  item: ProducedItem;
  // When the pipeline has more than one run, tag each row with its owning run
  // so a sub-bot's output is attributable.
  multiRun: boolean;
  onPreview: () => void;
}) {
  const label = `${producedKindLabel(item.kind)} · ${item.path}`;
  const dir = item.path.includes("/")
    ? item.path.slice(0, item.path.lastIndexOf("/"))
    : "";
  const consoleHref = `/runs/${encodeURIComponent(item.runId)}`;

  return (
    <li className="flex items-center gap-2 rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1.5">
      <KindIcon kind={item.kind} />
      <div className="min-w-0 flex-1">
        {item.source === "artifact" ? (
          <button
            type="button"
            onClick={onPreview}
            title={label}
            className="block max-w-full truncate text-left text-xs font-medium text-fg-default hover:text-accent-text hover:underline"
          >
            {item.name}
          </button>
        ) : (
          <Link
            href={consoleHref}
            title={`${label} — open in run console`}
            className="block max-w-full truncate text-xs font-medium text-fg-default hover:text-accent-text hover:underline"
          >
            {item.name}
          </Link>
        )}
        <div className="flex items-center gap-1.5 text-micro text-fg-subtle">
          {item.source === "artifact" ? (
            <>
              <Badge variant="accent">output</Badge>
              <span className="tabular-nums">{formatBytes(item.size ?? 0)}</span>
              {item.modifiedAt && (
                <span title={item.modifiedAt}>· {formatRelative(item.modifiedAt)}</span>
              )}
            </>
          ) : (
            <>
              <Badge variant="neutral">{item.lifecycle ?? "changed"}</Badge>
              {dir && (
                <span className="truncate" title={item.path}>
                  {dir}
                </span>
              )}
              {!item.binary && (
                <span className="tabular-nums">
                  <span className="text-success-fg">+{item.added ?? 0}</span>
                  <span className="text-fg-subtle"> </span>
                  <span className="text-danger-fg">-{item.deleted ?? 0}</span>
                </span>
              )}
              {item.binary && <span>(binary)</span>}
            </>
          )}
          {multiRun && (
            <Link
              href={consoleHref}
              className="font-mono text-accent-text hover:underline"
              title={`Produced by run ${item.runId}`}
            >
              · {item.runId.slice(0, 8)}
            </Link>
          )}
        </div>
      </div>
      {item.source === "artifact" ? (
        <div className="flex shrink-0 items-center gap-1">
          <Button variant="ghost" size="sm" onClick={onPreview}>
            Open
          </Button>
          <a
            href={`${artifactFileURL(item.runId, item.path)}?download=1`}
            download
            aria-label={`Download ${item.name}`}
            className="inline-flex h-6 w-6 items-center justify-center rounded-md text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
          >
            <DownloadIcon />
          </a>
        </div>
      ) : (
        <Link
          href={consoleHref}
          aria-label={`Open ${item.name} in the run console`}
          className="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
        >
          <ExternalLinkIcon />
        </Link>
      )}
    </li>
  );
}

// PreviewBody renders an inline preview for the fetched artifact: images,
// audio, and video play in-place; text shows in a <pre>; anything else falls
// back to the Download action in the dialog footer.
function PreviewBody({ preview }: { preview: PreviewState }) {
  if (preview.loading) {
    return (
      <div className="flex h-48 items-center justify-center gap-2 text-xs text-fg-subtle">
        <Spinner /> Loading preview…
      </div>
    );
  }
  if (preview.error) {
    return <div className="text-xs text-danger-fg">Failed to load: {preview.error}</div>;
  }
  if (preview.textBody !== null) {
    return (
      <pre className="max-h-[70vh] overflow-auto whitespace-pre-wrap break-words rounded bg-surface-0 p-3 font-mono text-xs">
        {preview.textBody}
      </pre>
    );
  }
  if (preview.blobURL) {
    if (preview.contentType.startsWith("image/")) {
      return (
        <div className="flex max-h-[70vh] items-center justify-center overflow-auto rounded bg-surface-0 p-3">
          <img src={preview.blobURL} alt={preview.path} className="max-w-full" />
        </div>
      );
    }
    if (preview.contentType.startsWith("audio/")) {
      return (
        <div className="flex items-center justify-center rounded bg-surface-0 p-6">
          <audio controls src={preview.blobURL} className="w-full">
            Your browser cannot play this audio file.
          </audio>
        </div>
      );
    }
    if (preview.contentType.startsWith("video/")) {
      return (
        <div className="flex items-center justify-center rounded bg-surface-0 p-3">
          <video controls src={preview.blobURL} className="max-h-[70vh] max-w-full">
            Your browser cannot play this video file.
          </video>
        </div>
      );
    }
  }
  return (
    <div className="py-6 text-center text-xs text-fg-subtle">
      Preview not available for this file type. Use Download to save it.
    </div>
  );
}

export default ProducedElements;
