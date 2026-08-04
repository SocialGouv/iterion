import { DownloadIcon, FileIcon } from "@radix-ui/react-icons";
import { useEffect, useState } from "react";

import { attachmentURL, fetchAttachment } from "@/api/runs";
import { ImagePreviewDialog } from "@/views/PipelineBoard/ImagePreview";
import { Spinner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { formatBytes } from "@/lib/format";
import type { GateInboundFileRef } from "@/lib/gateInbound";

interface Props {
  runId: string;
  file: GateInboundFileRef;
}

/**
 * One `file`-typed value of a gate's inbound payload, rendered as media
 * rather than as a path.
 *
 * The path a file descriptor carries is where the RUNNING NODES read the
 * bytes — a host path, or the sandbox bind-mount path inside the
 * container. Neither is reachable from a browser, so the only fetchable
 * handle is the attachment NAME, served by
 * `GET /api/runs/{id}/attachments/{name}`. A descriptor without one
 * (a bare `--answer x=@file` path, an LLM-answered gate) degrades to the
 * filename + path, which is still strictly more than the operator saw
 * before — never a broken image.
 */
export default function GateInboundFile({ runId, file }: Props) {
  const { blobURL, contentType, loading, error } = useAttachmentBlob(
    runId,
    file.attachment,
  );
  const [zoomed, setZoomed] = useState(false);
  const label = file.filename || file.attachment || file.path || "file";
  const mime = contentType || file.mime || "";

  const meta = (
    <span className="text-micro text-fg-subtle">
      {label}
      {file.size ? ` · ${formatBytes(file.size)}` : ""}
      {mime ? ` · ${mime}` : ""}
    </span>
  );

  if (!file.attachment) {
    // Not fetchable — show what we know instead of a dead <img>.
    return (
      <div className="flex items-center gap-1.5 text-micro text-fg-muted">
        <FileIcon className="shrink-0" />
        <code className="truncate" title={file.path || label}>
          {label}
        </code>
      </div>
    );
  }

  if (loading) {
    return (
      <div className="flex items-center gap-2 text-micro text-fg-subtle">
        <Spinner /> Loading {label}…
      </div>
    );
  }

  if (error || !blobURL) {
    return (
      <div className="space-y-0.5">
        <div className="text-micro text-danger-fg" role="alert">
          Couldn't load {label}: {error ?? "no content"}
        </div>
        <a
          href={attachmentURL(runId, file.attachment)}
          download={label}
          className="inline-flex items-center gap-1 text-micro text-accent-text hover:underline"
        >
          <DownloadIcon /> Download instead
        </a>
      </div>
    );
  }

  if (mime.startsWith("image/")) {
    return (
      <div className="space-y-1">
        <button
          type="button"
          onClick={() => setZoomed(true)}
          className="block max-w-full rounded border border-border-subtle bg-surface-0 p-1 hover:border-border-strong"
          title={`Open ${label}`}
        >
          <img src={blobURL} alt={label} className="max-h-64 max-w-full object-contain" />
        </button>
        {meta}
        {zoomed && (
          <ImagePreviewDialog
            open
            onOpenChange={(open) => {
              if (!open) setZoomed(false);
            }}
            src={blobURL}
            alt={label}
            title={<span className="font-mono text-xs">{label}</span>}
            description={meta}
            downloadHref={attachmentURL(runId, file.attachment)}
          />
        )}
      </div>
    );
  }

  if (mime.startsWith("audio/")) {
    return (
      <div className="space-y-1">
        <audio controls src={blobURL} className="w-full">
          Your browser cannot play this audio file.
        </audio>
        {meta}
      </div>
    );
  }

  if (mime.startsWith("video/")) {
    return (
      <div className="space-y-1">
        <video controls src={blobURL} className="max-h-64 max-w-full rounded">
          Your browser cannot play this video file.
        </video>
        {meta}
      </div>
    );
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <FileIcon className="shrink-0 text-fg-subtle" />
      {meta}
      <a
        href={attachmentURL(runId, file.attachment)}
        download={label}
        className="inline-flex items-center gap-1 text-micro text-accent-text hover:underline"
      >
        <DownloadIcon /> Download
      </a>
    </div>
  );
}

interface BlobState {
  blobURL: string | null;
  contentType: string;
  loading: boolean;
  error: string | null;
}

/**
 * Fetch an attachment into an ObjectURL, revoking it on unmount.
 *
 * A gate can disappear mid-fetch (the operator advances to the next
 * queued review, the run resumes), so the in-flight response is
 * invalidated on cleanup — otherwise it commits state on a dead
 * component and leaks an orphaned ObjectURL. Same discipline as
 * usePreview.
 *
 * Callers must give the component a `key` derived from the attachment so
 * a different file remounts it: the hook does not re-enter its loading
 * state when `name` changes under a live instance.
 */
function useAttachmentBlob(runId: string, name: string | undefined): BlobState {
  const [state, setState] = useState<BlobState>(() => ({
    blobURL: null,
    contentType: "",
    loading: !!name,
    error: null,
  }));

  useEffect(() => {
    if (!name) return;
    let cancelled = false;
    let created: string | null = null;
    fetchAttachment(runId, name)
      .then(({ blob, contentType }) => {
        if (cancelled) return;
        created = URL.createObjectURL(blob);
        setState({ blobURL: created, contentType, loading: false, error: null });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setState({
          blobURL: null,
          contentType: "",
          loading: false,
          error: errorMessage(err),
        });
      });
    return () => {
      cancelled = true;
      if (created) URL.revokeObjectURL(created);
    };
  }, [runId, name]);

  return state;
}
