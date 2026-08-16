import { DownloadIcon, MinusIcon, PlusIcon, ResetIcon } from "@radix-ui/react-icons";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

import { Dialog, IconButton } from "@/components/ui";

const ZOOM_MIN = 0.5;
const ZOOM_MAX = 5;
const ZOOM_STEP = 0.25;

export type MediaPreviewKind = "image" | "video" | "audio";

// MediaPreviewDialog is the /pipelines popup for every media attachment:
// image (zoom + pan), video, or audio. ImagePreviewDialog keeps the
// image-only call sites unchanged.
export function MediaPreviewDialog({
  open,
  onOpenChange,
  src,
  alt,
  title,
  description,
  downloadHref,
  footerExtra,
  kind = "image",
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  src: string;
  alt: string;
  title?: ReactNode;
  description?: ReactNode;
  downloadHref?: string;
  footerExtra?: ReactNode;
  kind?: MediaPreviewKind;
}) {
  if (kind !== "image") {
    return (
      <PlaybackPreviewDialog
        key={src}
        open={open}
        onOpenChange={onOpenChange}
        src={src}
        title={title}
        description={description}
        downloadHref={downloadHref}
        footerExtra={footerExtra}
        kind={kind}
      />
    );
  }
  return (
    <ImagePreviewDialog
      open={open}
      onOpenChange={onOpenChange}
      src={src}
      alt={alt}
      title={title}
      description={description}
      downloadHref={downloadHref}
      footerExtra={footerExtra}
    />
  );
}

function PlaybackPreviewDialog({
  open,
  onOpenChange,
  src,
  title,
  description,
  downloadHref,
  footerExtra,
  kind,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  src: string;
  title?: ReactNode;
  description?: ReactNode;
  downloadHref?: string;
  footerExtra?: ReactNode;
  kind: Exclude<MediaPreviewKind, "image">;
}) {
  const [loadError, setLoadError] = useState<string | null>(null);

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      widthClass={kind === "video" ? "max-w-5xl" : "max-w-3xl"}
      title={title}
      description={description}
      footer={
        <div className="flex w-full flex-wrap items-center justify-end gap-2">
          {footerExtra}
          {downloadHref && (
            <a
              href={downloadHref}
              download
              className="inline-flex items-center gap-1 rounded-md border border-border-default px-2.5 py-1 text-xs font-medium text-fg-default hover:bg-surface-2"
            >
              <DownloadIcon /> Download
            </a>
          )}
        </div>
      }
    >
      {loadError ? (
        <div className="py-6 text-center text-xs text-danger-fg">{loadError}</div>
      ) : kind === "video" ? (
        <div className="flex items-center justify-center rounded-md bg-surface-0 p-3">
          <video
            controls
            autoPlay
            src={src}
            className="max-h-[min(70vh,720px)] max-w-full"
            onError={() => setLoadError("Could not load this video file for playback.")}
          >
            Your browser cannot play this video file.
          </video>
        </div>
      ) : (
        <div className="flex items-center justify-center rounded-md bg-surface-0 p-6">
          <audio
            controls
            autoPlay
            src={src}
            className="w-full"
            onError={() => setLoadError("Could not load this audio file for playback.")}
          >
            Your browser cannot play this audio file.
          </audio>
        </div>
      )}
    </Dialog>
  );
}

// ImagePreviewDialog shows a full-viewport-safe image viewer with zoom
// (+/−, wheel, double-click) and pan-by-drag when zoomed. Used by ticket
// input thumbnails and produced-elements previews on /pipelines only.
export function ImagePreviewDialog({
  open,
  onOpenChange,
  src,
  alt,
  title,
  description,
  downloadHref,
  footerExtra,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  src: string;
  alt: string;
  title?: ReactNode;
  description?: ReactNode;
  downloadHref?: string;
  footerExtra?: ReactNode;
}) {
  const [zoom, setZoom] = useState(1);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const dragRef = useRef<{
    active: boolean;
    startX: number;
    startY: number;
    originX: number;
    originY: number;
    pointerId: number;
  } | null>(null);
  const viewportRef = useRef<HTMLDivElement>(null);

  // Reset zoom + pan each time a new image / open cycle starts.
  useEffect(() => {
    if (open) {
      setZoom(1);
      setPan({ x: 0, y: 0 });
    }
  }, [open, src]);

  const bump = useCallback((delta: number) => {
    setZoom((z) => {
      const next = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, Math.round((z + delta) * 100) / 100));
      if (next <= 1) setPan({ x: 0, y: 0 });
      return next;
    });
  }, []);

  const resetView = useCallback(() => {
    setZoom(1);
    setPan({ x: 0, y: 0 });
  }, []);

  const onWheel = useCallback(
    (e: React.WheelEvent) => {
      if (!(e.ctrlKey || e.metaKey)) return;
      e.preventDefault();
      bump(e.deltaY > 0 ? -ZOOM_STEP : ZOOM_STEP);
    },
    [bump],
  );

  const onPointerDown = useCallback(
    (e: React.PointerEvent) => {
      if (zoom <= 1) return;
      // Primary button / touch only.
      if (e.button !== 0 && e.pointerType === "mouse") return;
      e.preventDefault();
      (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
      dragRef.current = {
        active: true,
        startX: e.clientX,
        startY: e.clientY,
        originX: pan.x,
        originY: pan.y,
        pointerId: e.pointerId,
      };
    },
    [zoom, pan.x, pan.y],
  );

  const onPointerMove = useCallback((e: React.PointerEvent) => {
    const drag = dragRef.current;
    if (!drag?.active) return;
    setPan({
      x: drag.originX + (e.clientX - drag.startX),
      y: drag.originY + (e.clientY - drag.startY),
    });
  }, []);

  const endDrag = useCallback((e: React.PointerEvent) => {
    const drag = dragRef.current;
    if (!drag?.active) return;
    if (drag.pointerId === e.pointerId) {
      try {
        (e.currentTarget as HTMLElement).releasePointerCapture(e.pointerId);
      } catch {
        /* already released */
      }
      dragRef.current = null;
    }
  }, []);

  const canPan = zoom > 1;

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      widthClass="max-w-5xl"
      title={title}
      description={description}
      footer={
        <div className="flex w-full flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-1" role="group" aria-label="Zoom controls">
            <IconButton
              label="Zoom out"
              size="sm"
              variant="ghost"
              onClick={() => bump(-ZOOM_STEP)}
              disabled={zoom <= ZOOM_MIN}
            >
              <MinusIcon />
            </IconButton>
            <span className="min-w-[3.5rem] text-center font-mono text-xs text-fg-muted">
              {Math.round(zoom * 100)}%
            </span>
            <IconButton
              label="Zoom in"
              size="sm"
              variant="ghost"
              onClick={() => bump(ZOOM_STEP)}
              disabled={zoom >= ZOOM_MAX}
            >
              <PlusIcon />
            </IconButton>
            <IconButton
              label="Reset zoom and pan"
              size="sm"
              variant="ghost"
              onClick={resetView}
              disabled={zoom === 1 && pan.x === 0 && pan.y === 0}
            >
              <ResetIcon />
            </IconButton>
            <span className="ml-1 text-micro text-fg-subtle">
              Ctrl+scroll · drag to pan when zoomed · double-click reset
            </span>
          </div>
          <div className="flex items-center gap-2">
            {footerExtra}
            {downloadHref && (
              <a
                href={downloadHref}
                download
                className="inline-flex items-center gap-1 rounded-md border border-border-default px-2.5 py-1 text-xs font-medium text-fg-default hover:bg-surface-2"
              >
                <DownloadIcon /> Download
              </a>
            )}
          </div>
        </div>
      }
    >
      <div
        ref={viewportRef}
        onWheel={onWheel}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
        className={`flex min-h-[min(70vh,720px)] max-h-[min(70vh,720px)] items-center justify-center overflow-hidden rounded-md bg-surface-0 ${
          canPan ? "cursor-grab active:cursor-grabbing touch-none" : "cursor-default"
        }`}
        title={canPan ? "Drag to pan · Ctrl+scroll to zoom" : "Ctrl+scroll to zoom"}
      >
        <img
          src={src}
          alt={alt}
          draggable={false}
          onDoubleClick={resetView}
          className="max-w-none select-none object-contain"
          style={{
            maxHeight: "min(70vh, 720px)",
            maxWidth: "100%",
            transform: `translate(${pan.x}px, ${pan.y}px) scale(${zoom})`,
            transformOrigin: "center center",
            transition: dragRef.current?.active ? "none" : "transform 100ms ease-out",
          }}
        />
      </div>
    </Dialog>
  );
}
