import { DownloadIcon } from "@radix-ui/react-icons";

import { artifactFileURL } from "@/api/runs";
import type { PreviewState } from "@/components/Runs/usePreview";
import { Dialog, Spinner } from "@/components/ui";
import { formatBytes } from "@/lib/format";

import type { ProducedFileKind } from "./fileKind";
import { ImagePreviewDialog } from "./ImagePreview";

interface Props {
  preview: PreviewState;
  runId: string;
  kind: ProducedFileKind;
  /** Human-readable description for review images; paths remain the fallback. */
  imageAlt?: string;
  onClose: () => void;
}

// Shared authenticated artifact preview used by both the pipeline's produced
// elements and media explicitly attached to a human-review turn.
export function ArtifactFilePreviewDialog({
  preview,
  runId,
  kind,
  imageAlt,
  onClose,
}: Props) {
  const downloadHref = `${artifactFileURL(runId, preview.path)}?download=1`;
  const description = (
    <span>
      {formatBytes(preview.size)} · {preview.contentType || "loading…"}
    </span>
  );

  if (isImagePreview(preview, kind) && preview.blobURL) {
    return (
      <ImagePreviewDialog
        open
        onOpenChange={(open) => {
          if (!open) onClose();
        }}
        src={preview.blobURL}
        alt={imageAlt || preview.path}
        title={<span className="font-mono text-xs">{preview.path}</span>}
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
      title={<span className="font-mono text-xs">{preview.path}</span>}
      description={description}
      footer={
        <a
          href={downloadHref}
          download
          className="inline-flex items-center gap-1 rounded-md border border-border-default px-2.5 py-1 text-xs font-medium text-fg-default hover:bg-surface-2"
        >
          <DownloadIcon /> Download
        </a>
      }
    >
      <ArtifactPreviewBody preview={preview} kind={kind} imageAlt={imageAlt} />
    </Dialog>
  );
}

function isImagePreview(preview: PreviewState, kind: ProducedFileKind): boolean {
  if (preview.loading || preview.error || !preview.blobURL) return false;
  if (preview.textBody !== null) return false;
  return preview.contentType.startsWith("image/") || kind === "image";
}

// Content-Type wins over the declared/extension kind. The kind remains a
// compatibility fallback for stores that serve media as octet-stream.
export function ArtifactPreviewBody({
  preview,
  kind,
  imageAlt,
}: {
  preview: PreviewState;
  kind: ProducedFileKind;
  imageAlt?: string;
}) {
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
      <pre className="max-h-[70vh] overflow-auto whitespace-pre-wrap break-words rounded bg-surface-0 p-4 font-mono text-label leading-relaxed">
        {preview.textBody}
      </pre>
    );
  }
  if (preview.blobURL) {
    if (preview.contentType.startsWith("image/") || kind === "image") {
      return (
        <div className="flex max-h-[70vh] items-center justify-center overflow-auto rounded bg-surface-0 p-3">
          <img
            src={preview.blobURL}
            alt={imageAlt || preview.path}
            className="max-w-full"
          />
        </div>
      );
    }
    if (preview.contentType.startsWith("audio/") || kind === "audio") {
      return (
        <div className="flex items-center justify-center rounded bg-surface-0 p-6">
          <audio controls autoPlay src={preview.blobURL} className="w-full">
            Your browser cannot play this audio file.
          </audio>
        </div>
      );
    }
    if (preview.contentType.startsWith("video/") || kind === "video") {
      return (
        <div className="flex items-center justify-center rounded bg-surface-0 p-3">
          <video controls src={preview.blobURL} className="max-h-[70vh] max-w-full">
            Your browser cannot play this video file.
          </video>
        </div>
      );
    }
    if (preview.contentType === "application/pdf") {
      return (
        <iframe
          src={preview.blobURL}
          title={imageAlt || preview.path}
          className="h-[70vh] w-full rounded border-0 bg-surface-0"
        />
      );
    }
  }
  return (
    <div className="py-6 text-center text-xs text-fg-subtle">
      Preview not available for this file type. Use Download to save it.
    </div>
  );
}

export default ArtifactFilePreviewDialog;
