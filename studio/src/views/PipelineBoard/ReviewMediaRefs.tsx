import {
  ChevronLeftIcon,
  ChevronRightIcon,
  DownloadIcon,
  FileTextIcon,
  ImageIcon,
  SpeakerLoudIcon,
  TableIcon,
  VideoIcon,
} from "@radix-ui/react-icons";
import { type KeyboardEvent, useState } from "react";

import { artifactFileURL } from "@/api/runs";
import type {
  PipelineBoardReviewMedia,
  PipelineBoardReviewMediaKind,
} from "@/api/pipelineBoards";
import { usePreview, type PreviewState } from "@/components/Runs/usePreview";
import { Badge, Button } from "@/components/ui";
import { formatBytes } from "@/lib/format";

import { ArtifactFilePreviewDialog } from "./ArtifactFilePreviewDialog";

interface Props {
  media: readonly PipelineBoardReviewMedia[];
  fallbackRunId: string;
}

interface SelectedMedia {
  runId: string;
  kind: PipelineBoardReviewMediaKind;
  imageAlt?: string;
  directPreview?: PreviewState;
}

// ReviewMediaRefs renders only the files explicitly attached to the current
// AI↔human turn. The wire contains run-id/path metadata, never an arbitrary
// URL; preview/download URLs are constructed through the authenticated run
// artifact endpoint here.
export function ReviewMediaRefs({ media, fallbackRunId }: Props) {
  if (media.length === 0) return null;

  // Remount the stateful carousel when the exact ordered attachment payload
  // changes. Board polling commonly replaces the array with equivalent data;
  // the stable content key preserves the current slide in that case, while a
  // genuinely new review turn cannot inherit an old index or open preview.
  const contentKey = JSON.stringify([
    fallbackRunId,
    media.map((ref) => [
      ref.run_id ?? "",
      ref.path,
      ref.kind,
      ref.mime ?? "",
      ref.size ?? null,
      ref.caption ?? "",
    ]),
  ]);

  return (
    <ReviewMediaCarousel
      key={contentKey}
      media={media}
      fallbackRunId={fallbackRunId}
    />
  );
}

function ReviewMediaCarousel({ media, fallbackRunId }: Props) {
  const [activeIndex, setActiveIndex] = useState(0);
  const [selected, setSelected] = useState<SelectedMedia | null>(null);
  const { preview, openPreview, closePreview } = usePreview(fallbackRunId || null);

  const activeRef = media[activeIndex];
  if (!activeRef) return null;
  const selectedPreview = selected?.directPreview ?? preview;

  const handleOpen = (ref: PipelineBoardReviewMedia) => {
    const runId = ref.run_id || fallbackRunId;
    if (!runId) return;
    if (ref.kind === "doc" || ref.kind === "data") {
      setSelected({
        runId,
        kind: ref.kind,
        imageAlt: ref.caption,
      });
      openPreview({ path: ref.path, size: ref.size ?? 0 }, runId);
      return;
    }
    // Review attachments are already constrained to passive browser-media
    // formats by the runtime. Give the player the authenticated artifact URL
    // directly so audio/video can start loading progressively instead of
    // waiting for usePreview to buffer the complete response into a Blob.
    setSelected({
      runId,
      kind: ref.kind,
      imageAlt: ref.caption,
      directPreview: {
        path: ref.path,
        size: ref.size ?? 0,
        loading: false,
        error: null,
        textBody: null,
        blobURL: artifactFileURL(runId, ref.path),
        contentType: ref.mime || "application/octet-stream",
      },
    });
  };

  const showMedia = (index: number) => {
    setActiveIndex(Math.min(Math.max(index, 0), media.length - 1));
  };

  const handleCarouselKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    let nextIndex: number;
    switch (event.key) {
      case "ArrowLeft":
        nextIndex = activeIndex - 1;
        break;
      case "ArrowRight":
        nextIndex = activeIndex + 1;
        break;
      case "Home":
        nextIndex = 0;
        break;
      case "End":
        nextIndex = media.length - 1;
        break;
      default:
        return;
    }
    event.preventDefault();
    event.stopPropagation();
    showMedia(nextIndex);
  };

  const runId = activeRef.run_id || fallbackRunId;
  const downloadHref = runId
    ? `${artifactFileURL(runId, activeRef.path)}?download=1`
    : undefined;
  const fileName = activeRef.path.split("/").pop() || activeRef.path;
  const label = activeRef.caption || defaultMediaLabel(activeRef.kind);
  const metadata = [
    activeRef.mime,
    activeRef.size !== undefined ? formatBytes(activeRef.size) : undefined,
  ]
    .filter(Boolean)
    .join(" · ");

  return (
    <section
      aria-label="Attached files to review"
      className="space-y-3 rounded-lg border border-border-default bg-surface-1 p-3"
    >
      <header className="space-y-0.5">
        <div className="flex items-center gap-2">
          <h3 className="text-sm font-semibold text-fg-default">
            Review these files
          </h3>
          <Badge variant="neutral">{media.length}</Badge>
        </div>
        <p className="text-xs leading-relaxed text-fg-muted">
          Open each attached file here before answering.
        </p>
      </header>

      <div
        role="group"
        aria-label="Attached review media carousel"
        aria-roledescription="carousel"
        aria-keyshortcuts="ArrowLeft ArrowRight Home End"
        tabIndex={0}
        onKeyDown={handleCarouselKeyDown}
        className="space-y-3 rounded-lg outline-none focus-visible:ring-2 focus-visible:ring-accent"
      >
        <div
          role="group"
          aria-roledescription="slide"
          aria-label={`File ${activeIndex + 1} of ${media.length}: ${label}`}
        >
          {activeRef.kind === "image" && runId ? (
            <div className="group relative min-w-0 overflow-hidden rounded-lg border border-border-subtle bg-surface-0">
              <button
                type="button"
                onClick={() => handleOpen(activeRef)}
                className="block w-full text-left transition-colors hover:bg-surface-2"
                aria-label={`Open ${label}`}
              >
                <div className="flex aspect-[16/10] max-h-80 w-full items-center justify-center bg-surface-2/40 p-2">
                  <img
                    src={artifactFileURL(runId, activeRef.path)}
                    alt={label}
                    loading="lazy"
                    className="max-h-full max-w-full object-contain transition-transform group-hover:scale-[1.01]"
                  />
                </div>
                <div className="flex items-center gap-3 border-t border-border-subtle px-3 py-3">
                  <div className="min-w-0 flex-1 space-y-0.5">
                    <p className="text-sm font-medium text-fg-default">
                      {label}
                    </p>
                    <p
                      className="truncate text-xs text-fg-muted"
                      title={activeRef.path}
                    >
                      {fileName}
                    </p>
                  </div>
                  <span className="shrink-0 text-xs font-medium text-accent-text">
                    Open full-size image
                  </span>
                </div>
              </button>
              {downloadHref && (
                <DownloadLink href={downloadHref} path={activeRef.path} overlay />
              )}
            </div>
          ) : (
            <div
              className="relative flex min-h-64 w-full flex-col items-center justify-center overflow-hidden rounded-lg border border-border-subtle bg-surface-0"
            >
              <button
                type="button"
                onClick={() => handleOpen(activeRef)}
                disabled={!runId}
                aria-label={`Open ${label}`}
                className="flex min-h-64 w-full flex-col items-center justify-center gap-4 px-6 py-10 text-center transition-colors hover:bg-surface-2 disabled:cursor-not-allowed disabled:opacity-60"
              >
                <MediaKindIcon
                  kind={activeRef.kind}
                  className="h-14 w-14 text-accent-text"
                />
                <span className="space-y-1">
                  <span className="block whitespace-pre-wrap break-words text-lg font-semibold text-fg-default">
                    {label}
                  </span>
                  <span
                    className="block break-all text-sm text-fg-muted"
                    title={activeRef.path}
                  >
                    {fileName}
                  </span>
                  {metadata && (
                    <span className="block text-xs text-fg-subtle">
                      {metadata}
                    </span>
                  )}
                </span>
                <span className="text-sm font-medium text-accent-text">
                  {runId ? "Open preview" : "Preview unavailable"}
                </span>
              </button>
              {downloadHref && (
                <DownloadLink
                  href={downloadHref}
                  path={activeRef.path}
                  overlay
                />
              )}
            </div>
          )}
        </div>

        {media.length > 1 ? (
          <div className="flex items-center justify-between gap-3">
            <Button
              variant="secondary"
              size="sm"
              onClick={() => showMedia(activeIndex - 1)}
              disabled={activeIndex === 0}
              aria-label="Previous review media"
            >
              <ChevronLeftIcon aria-hidden />
              Previous
            </Button>
            <span
              className="text-sm font-medium tabular-nums text-fg-muted"
              aria-live="polite"
              aria-atomic="true"
            >
              {activeIndex + 1} / {media.length}
            </span>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => showMedia(activeIndex + 1)}
              disabled={activeIndex === media.length - 1}
              aria-label="Next review media"
            >
              Next
              <ChevronRightIcon aria-hidden />
            </Button>
          </div>
        ) : (
          <span
            className="block text-center text-sm font-medium tabular-nums text-fg-muted"
            aria-label="1 attached review file"
          >
            1 / 1
          </span>
        )}

        {media.length > 1 && media.length <= 10 && (
          <div
            role="group"
            aria-label="Choose attached review media"
            className="flex flex-wrap justify-center gap-2"
          >
            {media.map((ref, index) => (
              <button
                key={`${ref.run_id || fallbackRunId}:${ref.path}:${index}`}
                type="button"
                onClick={() => showMedia(index)}
                aria-label={`Show media ${index + 1}: ${
                  ref.caption || defaultMediaLabel(ref.kind)
                }`}
                aria-current={index === activeIndex ? "true" : undefined}
                className={`h-2.5 w-2.5 rounded-full border transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent ${
                  index === activeIndex
                    ? "border-accent bg-accent"
                    : "border-border-strong bg-surface-2 hover:bg-surface-3"
                }`}
              />
            ))}
          </div>
        )}
      </div>

      {selected && selectedPreview && (
        <ArtifactFilePreviewDialog
          preview={selectedPreview}
          runId={selected.runId}
          kind={selected.kind}
          imageAlt={selected.imageAlt}
          onClose={() => {
            setSelected(null);
            closePreview();
          }}
        />
      )}
    </section>
  );
}

function DownloadLink({
  href,
  path,
  overlay = false,
}: {
  href: string;
  path: string;
  overlay?: boolean;
}) {
  return (
    <a
      href={href}
      download
      aria-label={`Download ${path}`}
      className={
        overlay
          ? "absolute right-3 top-3 inline-flex h-9 w-9 items-center justify-center rounded-md border border-border-subtle bg-surface-1/90 text-fg-muted shadow-[var(--shadow-sm)] hover:text-fg-default"
          : "inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
      }
    >
      <DownloadIcon />
    </a>
  );
}

function defaultMediaLabel(kind: PipelineBoardReviewMediaKind): string {
  switch (kind) {
    case "audio":
      return "Review audio";
    case "video":
      return "Review video";
    case "doc":
      return "Review document";
    case "data":
      return "Review data";
    default:
      return "Review image";
  }
}

function MediaKindIcon({
  kind,
  className = "h-4 w-4 shrink-0 text-fg-muted",
}: {
  kind: PipelineBoardReviewMediaKind;
  className?: string;
}) {
  switch (kind) {
    case "audio":
      return <SpeakerLoudIcon className={className} aria-hidden />;
    case "video":
      return <VideoIcon className={className} aria-hidden />;
    case "doc":
      return <FileTextIcon className={className} aria-hidden />;
    case "data":
      return <TableIcon className={className} aria-hidden />;
    default:
      return <ImageIcon className={className} aria-hidden />;
  }
}

export default ReviewMediaRefs;
