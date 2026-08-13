import { DownloadIcon, FileIcon } from "@radix-ui/react-icons";
import { useEffect, useMemo, useRef, useState } from "react";

import { attachmentURL, fetchAttachment } from "@/api/runs";
import { ImagePreviewDialog } from "@/views/PipelineBoard/ImagePreview";
import { CopyButton, Spinner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { formatBytes } from "@/lib/format";
import type { GateInboundFileRef } from "@/lib/gateInbound";
import { prettyJSON, textPreviewKind } from "@/lib/textPreview";

import {
  COLLAPSE_AFTER_LINES,
  COLLAPSED_MAX_HEIGHT,
  MARKDOWN_PARSE_BUDGET,
  TEXT_PREVIEW_BYTE_BUDGET,
  Toggle,
} from "./gateInboundFold";
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
  const kind = classifyAttachmentPreview(contentType, file.mime, label);

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

  if (error) {
    return (
      <div className="space-y-0.5">
        <div className="text-micro text-danger-fg" role="alert">
          Couldn't load {label}: {error}
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

function TextBody({ kind, text }: { kind: NonNullable<ReturnType<typeof textPreviewKind>>; text: string }) {
  // Inlined bodies are already ≤ TEXT_PREVIEW_BYTE_BUDGET, so pretty-print
  // every JSON attachment — text.length cannot exceed the byte cap.
  const shown = useMemo(() => (kind === "json" ? prettyJSON(text) : text), [kind, text]);
  const lines = useMemo(() => shown.split("\n"), [shown]);
  const long = lines.length > COLLAPSE_AFTER_LINES || shown.length > MARKDOWN_PARSE_BUDGET;
  const [open, setOpen] = useState(!long);
  const folded = long && !open;
  const heavyMd = kind === "markdown" && shown.length > MARKDOWN_PARSE_BUDGET;
  // Markdown keeps the CSS clamp (slicing mid-fence swallows the rest).
  // json/text slice so a multi-MB log never enters the folded <pre>.
  const preview = folded
    ? lines.slice(0, COLLAPSE_AFTER_LINES).join("\n").slice(0, MARKDOWN_PARSE_BUDGET)
    : shown;

  return (
    <div className="min-w-0">
      <div className={kind === "json" ? "flex items-start gap-1" : undefined}>
        <div
          className={folded ? "relative min-w-0 flex-1 overflow-hidden" : "min-w-0 flex-1"}
          style={folded ? { maxHeight: COLLAPSED_MAX_HEIGHT } : undefined}
        >
          {kind === "markdown" && !(folded && heavyMd) ? (
            <MarkdownText value={shown} size="sm" />
          ) : (
            <pre className="min-w-0 overflow-x-auto whitespace-pre-wrap break-words rounded bg-surface-0 p-1.5 font-mono text-micro leading-relaxed">
              {folded ? preview : shown}
            </pre>
          )}
          {folded && (
            <div
              aria-hidden="true"
              className="pointer-events-none absolute inset-x-0 bottom-0 h-8 bg-gradient-to-b from-transparent to-surface-1"
            />
          )}
        </div>
        {kind === "json" && (
          <CopyButton value={shown} variant="icon" label="Copy JSON" copiedLabel="Copied" />
        )}
      </div>
      {long && (
        <Toggle
          open={open}
          onToggle={() => setOpen((v) => !v)}
          closedLabel={
            lines.length > COLLAPSE_AFTER_LINES ? `Show all ${lines.length} lines` : "Show all"
          }
        />
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

/** fetchAttachment defaults a missing header to octet-stream, so
 *  `contentType || declaredMime` never sees the descriptor. Fall back
 *  only when the server type is generic; a real image/zip stays media. */
function classifyAttachmentPreview(
  contentType: string,
  declaredMime: string | undefined,
  filename: string,
): ReturnType<typeof textPreviewKind> {
  const fromServer = textPreviewKind(contentType, filename);
  if (fromServer) return fromServer;
  const [rawType] = contentType.toLowerCase().split(";");
  const ct = (rawType ?? "").trim();
  if (ct !== "" && ct !== "application/octet-stream") return null;
  return textPreviewKind(declaredMime ?? "", filename);
}

/**
 * Fetch an attachment as text (documents / JSON) or as an ObjectURL
 * (media). Same generation discipline as usePreview: a gate can
 * disappear mid-fetch.
 *
 * Effect deps are runId+name only. Filename and declaredMime feed
 * classification through refs. The parent keys this component on the
 * attachment name, so a different file remounts; changing mime under a
 * live instance must not revoke a still-rendered ObjectURL.
 */
function useAttachmentBlob(
  runId: string,
  name: string | undefined,
  filename: string,
  declaredMime: string | undefined,
): BlobState {
  const filenameRef = useRef(filename);
  const declaredMimeRef = useRef(declaredMime);
  filenameRef.current = filename;
  declaredMimeRef.current = declaredMime;

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
    setState({
      blobURL: null,
      textBody: null,
      contentType: "",
      loading: true,
      error: null,
    });
    fetchAttachment(runId, name)
      .then(async ({ blob, contentType }) => {
        if (cancelled) return;
        const kind = classifyAttachmentPreview(
          contentType,
          declaredMimeRef.current,
          filenameRef.current,
        );
        if (kind) {
          if (blob.size > TEXT_PREVIEW_BYTE_BUDGET) {
            // Too large to decode into a JS string. Fall through to
            // the download row — do not ObjectURL a text body.
            setState({
              blobURL: null,
              textBody: null,
              contentType,
              loading: false,
              error: null,
            });
            return;
          }
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
  }, [runId, name]);

  return state;
}
