import { ChevronDownIcon, ChevronRightIcon, DownloadIcon, FileIcon } from "@radix-ui/react-icons";
import { useEffect, useMemo, useState } from "react";

import { attachmentURL, fetchAttachment } from "@/api/runs";
import { ImagePreviewDialog } from "@/views/PipelineBoard/ImagePreview";
import { Spinner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { formatBytes } from "@/lib/format";
import type { GateInboundFileRef } from "@/lib/gateInbound";
import { prettyJSON, textPreviewKind } from "@/lib/textPreview";

import MarkdownText from "./MarkdownText";

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
 *
 * Text, markdown and JSON are shown inline (same fold as the inbound
 * payload renderer). Images, audio and video keep their media players.
 * Anything else stays a download — a zip at a gate is not a preview.
 */
export default function GateInboundFile({ runId, file }: Props) {
  const label = file.filename || file.attachment || file.path || "file";
  const { blobURL, textBody, contentType, loading, error } = useAttachmentBlob(
    runId,
    file.attachment,
    label,
    file.mime,
  );
  const [zoomed, setZoomed] = useState(false);
  const mime = contentType || file.mime || "";
  const kind = textPreviewKind(mime, label);

  const meta = (
    <span className="text-micro text-fg-subtle">
      {label}
      {file.size ? ` · ${formatBytes(file.size)}` : ""}
      {mime ? ` · ${mime}` : ""}
    </span>
  );

  const download = file.attachment ? (
    <a
      href={attachmentURL(runId, file.attachment)}
      download={label}
      className="inline-flex items-center gap-1 text-micro text-accent-text hover:underline"
    >
      <DownloadIcon /> Download
    </a>
  ) : null;

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

  if (error || (!blobURL && textBody === null)) {
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

  if (textBody !== null && kind) {
    return (
      <div className="space-y-1">
        <TextBody kind={kind} text={textBody} />
        <div className="flex flex-wrap items-center gap-2">
          {meta}
          {download}
        </div>
      </div>
    );
  }

  if (blobURL && mime.startsWith("image/")) {
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

  if (blobURL && mime.startsWith("audio/")) {
    return (
      <div className="space-y-1">
        <audio controls src={blobURL} className="w-full">
          Your browser cannot play this audio file.
        </audio>
        {meta}
      </div>
    );
  }

  if (blobURL && mime.startsWith("video/")) {
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
      {download}
    </div>
  );
}

// Matches GateInboundPayload: enough to read a short plan, short enough
// that two files plus the answer form still fit on screen.
const COLLAPSE_AFTER_LINES = 12;
const COLLAPSED_MAX_HEIGHT = "16rem";
const MARKDOWN_PARSE_BUDGET = 20_000;

function TextBody({ kind, text }: { kind: NonNullable<ReturnType<typeof textPreviewKind>>; text: string }) {
  const shown = useMemo(() => (kind === "json" ? prettyJSON(text) : text), [kind, text]);
  const lines = shown.split("\n");
  const long = lines.length > COLLAPSE_AFTER_LINES;
  const [open, setOpen] = useState(!long);
  const folded = long && !open;
  const heavy = kind === "markdown" && shown.length > MARKDOWN_PARSE_BUDGET;

  return (
    <div className="min-w-0">
      <div
        className={folded ? "relative overflow-hidden" : undefined}
        style={folded ? { maxHeight: COLLAPSED_MAX_HEIGHT } : undefined}
      >
        {kind === "markdown" && !(folded && heavy) ? (
          <MarkdownText value={shown} size="sm" />
        ) : (
          <pre className="min-w-0 overflow-x-auto whitespace-pre-wrap break-words rounded bg-surface-0 p-1.5 font-mono text-micro leading-relaxed">
            {folded && heavy ? shown.slice(0, MARKDOWN_PARSE_BUDGET) : shown}
          </pre>
        )}
        {folded && (
          <div
            aria-hidden="true"
            className="pointer-events-none absolute inset-x-0 bottom-0 h-8 bg-gradient-to-b from-transparent to-surface-1"
          />
        )}
      </div>
      {long && (
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          aria-expanded={open}
          className="inline-flex items-center gap-0.5 text-micro text-accent-text hover:underline"
        >
          {open ? <ChevronDownIcon /> : <ChevronRightIcon />}
          {open ? "Show less" : `Show all ${lines.length} lines`}
        </button>
      )}
    </div>
  );
}

interface BlobState {
  blobURL: string | null;
  textBody: string | null;
  contentType: string;
  loading: boolean;
  error: string | null;
}

/**
 * Fetch an attachment as text (documents / JSON) or as an ObjectURL
 * (media). Same generation discipline as usePreview: a gate can
 * disappear mid-fetch.
 *
 * Callers must give the component a `key` derived from the attachment so
 * a different file remounts it: the hook does not re-enter its loading
 * state when `name` changes under a live instance.
 */
function useAttachmentBlob(
  runId: string,
  name: string | undefined,
  filename: string,
  declaredMime: string | undefined,
): BlobState {
  const [state, setState] = useState<BlobState>(() => ({
    blobURL: null,
    textBody: null,
    contentType: "",
    loading: !!name,
    error: null,
  }));

  useEffect(() => {
    if (!name) return;
    let cancelled = false;
    let created: string | null = null;
    fetchAttachment(runId, name)
      .then(async ({ blob, contentType }) => {
        if (cancelled) return;
        const kind = textPreviewKind(contentType || declaredMime || "", filename);
        if (kind) {
          const textBody = await blob.text();
          if (cancelled) return;
          setState({
            blobURL: null,
            textBody,
            contentType,
            loading: false,
            error: null,
          });
          return;
        }
        created = URL.createObjectURL(blob);
        setState({
          blobURL: created,
          textBody: null,
          contentType,
          loading: false,
          error: null,
        });
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setState({
          blobURL: null,
          textBody: null,
          contentType: "",
          loading: false,
          error: errorMessage(err),
        });
      });
    return () => {
      cancelled = true;
      if (created) URL.revokeObjectURL(created);
    };
  }, [runId, name, filename, declaredMime]);

  return state;
}
