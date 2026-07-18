import {
  ChevronLeftIcon,
  ChevronRightIcon,
  Cross2Icon,
  DownloadIcon,
} from "@radix-ui/react-icons";
import { useState } from "react";
import { Link } from "wouter";

import { pipelineBoardImageURL, type PipelineBoardCard } from "@/api/pipelineBoards";
import type { UnifiedStatus } from "@/components/Runs/runStatusClasses";
import { Badge, Dialog, IconButton, InlineBanner, StatusBadge } from "@/components/ui";

import { ProducedElements } from "./ProducedElements";
import { SequentialReviews } from "./SequentialReviews";

const KNOWN_STATUSES = new Set<UnifiedStatus>([
  "running",
  "paused_waiting_human",
  "paused_operator",
  "finished",
  "failed",
  "failed_resumable",
  "cancelled",
  "queued",
  "skipped",
  "none",
]);

function StatusChip({ status }: { status?: string }) {
  if (!status) return null;
  return KNOWN_STATUSES.has(status as UnifiedStatus) ? (
    <StatusBadge status={status as UnifiedStatus} />
  ) : (
    <Badge>{status}</Badge>
  );
}

function stringifyValue(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

const IMAGE_PATH_RE = /\.(png|jpe?g|webp|gif)$/i;

// imagePathsFrom extracts workdir-relative image paths from one input value:
// either a JSON array of image paths (how bots ship reference-image lists in
// bot_args) or a single bare path. Mixed arrays and anything else yield []
// so the value falls back to the plain monospace rendering — a thumbnail
// must never hide non-image content.
function imagePathsFrom(value: unknown): string[] {
  const asPathList = (items: unknown[]): string[] => {
    const paths = items.filter(
      (item): item is string => typeof item === "string" && IMAGE_PATH_RE.test(item.trim()),
    );
    return paths.length > 0 && paths.length === items.length
      ? paths.map((p) => p.trim())
      : [];
  };
  if (Array.isArray(value)) return asPathList(value);
  if (typeof value !== "string") return [];
  const text = value.trim();
  if (!text || text.length > 4096) return [];
  if (text.startsWith("[")) {
    try {
      const parsed: unknown = JSON.parse(text);
      return Array.isArray(parsed) ? asPathList(parsed) : [];
    } catch {
      return [];
    }
  }
  // Bare-path case: a single token only — a SENTENCE that happens to end in
  // ".png" must stay plain text.
  return IMAGE_PATH_RE.test(text) && !/\s/.test(text) ? [text] : [];
}

// InputImageCarousel renders an image-path input value as one image at a
// time with prev/next cycling — reference lists carry several views of the
// same character, so a carousel keeps the sidebar compact while every image
// stays reachable. Clicking the image opens the SAME preview dialog as the
// produced-elements viewer (large image + Download), with the carousel
// arrows still available inside it. A failed load (deleted file, foreign
// workdir) collapses back to the plain monospace text so no information is
// ever lost.
function InputImageCarousel({ paths, rawText }: { paths: string[]; rawText: string }) {
  const [index, setIndex] = useState(0);
  const [broken, setBroken] = useState(false);
  const [viewerOpen, setViewerOpen] = useState(false);
  const current = ((index % paths.length) + paths.length) % paths.length;
  const path = paths[current];
  if (broken || path === undefined) {
    return (
      <dd className="whitespace-pre-wrap break-words rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1 font-mono text-xs text-fg-default">
        {rawText}
      </dd>
    );
  }
  const url = pipelineBoardImageURL(path);
  const fileName = path.split("/").pop() ?? path;
  const cycler = (dir: -1 | 1, size: "sm" | "md" = "sm") =>
    paths.length > 1 ? (
      <IconButton
        label={dir < 0 ? "Previous image" : "Next image"}
        size={size}
        variant="ghost"
        onClick={() => setIndex((i) => i + dir)}
      >
        {dir < 0 ? <ChevronLeftIcon /> : <ChevronRightIcon />}
      </IconButton>
    ) : (
      <span />
    );
  return (
    <dd className="space-y-1 rounded-md border border-border-subtle bg-surface-2/40 p-2">
      <button
        type="button"
        onClick={() => setViewerOpen(true)}
        title={`${path} — open preview`}
        className="block w-full"
      >
        <img
          src={url}
          alt={path}
          loading="lazy"
          className="max-h-44 w-full rounded object-contain"
          onError={() => setBroken(true)}
        />
      </button>
      <div className="flex items-center justify-between gap-1">
        {cycler(-1)}
        <span
          className="min-w-0 truncate font-mono text-micro text-fg-muted"
          title={path}
        >
          {fileName}
          {paths.length > 1 ? ` · ${current + 1}/${paths.length}` : ""}
        </span>
        {cycler(1)}
      </div>
      {viewerOpen && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) setViewerOpen(false);
          }}
          widthClass="max-w-3xl"
          title={<span className="font-mono text-xs">{path}</span>}
          description={
            paths.length > 1 ? <span>Image {current + 1}/{paths.length}</span> : undefined
          }
          footer={
            <a
              href={url}
              download
              className="inline-flex items-center gap-1 rounded-md border border-border-default px-2.5 py-1 text-xs font-medium text-fg-default hover:bg-surface-2"
            >
              <DownloadIcon /> Download
            </a>
          }
        >
          <div className="space-y-2">
            <div className="flex max-h-[70vh] items-center justify-center overflow-auto rounded bg-surface-0 p-3">
              <img src={url} alt={path} className="max-w-full" />
            </div>
            {paths.length > 1 && (
              <div className="flex items-center justify-center gap-3">
                {cycler(-1, "md")}
                <span className="font-mono text-xs text-fg-muted">
                  {current + 1}/{paths.length}
                </span>
                {cycler(1, "md")}
              </div>
            )}
          </div>
        </Dialog>
      )}
    </dd>
  );
}

// InputsList renders the pipeline's full entry input (launch vars / task
// bot-args) as an untruncated key → value list. The sidebar has the room the
// compact card body does not, so values wrap instead of clipping. Values that
// are image paths (reference-image lists…) render as an inline carousel
// instead of bare paths.
function InputsList({ input }: { input?: Record<string, unknown> }) {
  const entries = input ? Object.entries(input) : [];
  if (entries.length === 0) {
    return <p className="text-xs italic text-fg-subtle">No inputs recorded.</p>;
  }
  return (
    <dl className="space-y-2">
      {entries.map(([key, value]) => {
        const imagePaths = imagePathsFrom(value);
        return (
          <div key={key} className="space-y-0.5">
            <dt className="text-micro font-medium uppercase tracking-wide text-fg-muted">
              {key}
            </dt>
            {imagePaths.length > 0 ? (
              <InputImageCarousel paths={imagePaths} rawText={stringifyValue(value)} />
            ) : typeof value === "object" && value !== null ? (
              // Structured values get a scrollable pretty-printed JSON block;
              // scalars keep the plain wrapped rendering below.
              <dd className="m-0">
                <pre className="m-0 max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1 font-mono text-micro text-fg-default">
                  {stringifyValue(value)}
                </pre>
              </dd>
            ) : (
              <dd className="whitespace-pre-wrap break-words rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1 font-mono text-xs text-fg-default">
                {stringifyValue(value)}
              </dd>
            )}
          </div>
        );
      })}
    </dl>
  );
}

function MetaRow({ card }: { card: PipelineBoardCard }) {
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-caption text-fg-subtle">
      <StatusChip status={card.status} />
      {!!card.priority && card.priority > 0 && (
        <Badge
          variant="warning"
          title={`Priority ${card.priority} — higher numbers launch first from Todo`}
        >
          P{card.priority}
        </Badge>
      )}
      {card.bot_id && <code title={`Bot ${card.bot_id}`}>{card.bot_id}</code>}
      {card.workflow_name && <span className="truncate">{card.workflow_name}</span>}
      {card.issue_id && <code title={`Issue ${card.issue_id}`}>#{card.issue_id}</code>}
      {card.run_id && (
        <Link
          href={`/runs/${encodeURIComponent(card.run_id)}`}
          className="ml-auto font-mono text-accent-text hover:underline"
          title={`Open run ${card.run_id} in the run console`}
        >
          Open run console →
        </Link>
      )}
    </div>
  );
}

function SectionHeading({ children }: { children: React.ReactNode }) {
  return <h3 className="text-xs font-semibold text-fg-default">{children}</h3>;
}

// PipelineCardDetailsBody composes the sidebar sections by lane:
//   - Todo / Backlog (no run started)   → Inputs only.
//   - In progress                        → Inputs + Produced elements.
//   - In progress, awaiting human input  → + the response form (first, so the
//                                          operator's action is front and centre).
//   - Done                               → Inputs + Result + Produced elements.
//   - Failed                             → Failure reason + Inputs + Produced
//                                          elements (what it made before dying).
// Exported (separately from the panel wrapper) so it can be unit-tested in
// isolation from the docked-panel chrome.
export function PipelineCardDetailsBody({
  card,
  stale,
  onRefetch,
}: {
  card: PipelineBoardCard;
  // The card vanished from the latest projection (started / finished / folded)
  // — we keep showing the last known snapshot with a heads-up banner.
  stale?: boolean;
  onRefetch: () => void;
}) {
  const reviews = card.pending_reviews ?? [];
  const showProduced =
    !!card.run_id &&
    (card.column_id === "in_progress" ||
      card.column_id === "done" ||
      card.column_id === "failed");
  // The whole pipeline tree — root + sub-bots — so a sub-bot's produced
  // elements surface here too. Falls back to the root run for older
  // projections that don't emit the tree.
  const treeRunIds =
    card.tree_run_ids && card.tree_run_ids.length > 0
      ? card.tree_run_ids
      : card.run_id
        ? [card.run_id]
        : [];

  return (
    <div className="space-y-5">
      {stale && (
        <InlineBanner tone="warning" layout="inline">
          This card is no longer on the board — it may have started, finished,
          or been folded into another pipeline. Showing the last known state.
        </InlineBanner>
      )}

      <MetaRow card={card} />

      {reviews.length > 0 && (
        <section aria-label="Response required" className="space-y-2">
          <SectionHeading>Response required</SectionHeading>
          {card.failed && (
            <InlineBanner tone="warning" layout="inline">
              This pipeline's run was interrupted (e.g. a server restart).
              Answering the reviews below resumes the child runs, but the
              pipeline itself needs a resume from its run console to pick up
              their results and finish.
            </InlineBanner>
          )}
          <SequentialReviews card={card} onResolved={onRefetch} />
        </section>
      )}

      {card.column_id === "failed" && card.error && (
        <section aria-label="Failure" className="space-y-2">
          <SectionHeading>Failure</SectionHeading>
          <InlineBanner tone="danger" layout="inline">
            {card.error}
          </InlineBanner>
        </section>
      )}

      <section aria-label="Inputs" className="space-y-2">
        <SectionHeading>Inputs</SectionHeading>
        <InputsList input={card.entry_input} />
      </section>

      {card.column_id === "done" && card.output && (
        <section aria-label="Result" className="space-y-2">
          <SectionHeading>Result</SectionHeading>
          <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border-default bg-surface-0 p-2 font-mono text-xs text-fg-muted">
            {card.output}
          </pre>
        </section>
      )}

      {showProduced && treeRunIds.length > 0 && (
        <ProducedElements runIds={treeRunIds} status={card.status} />
      )}
    </div>
  );
}

interface Props {
  card: PipelineBoardCard;
  stale?: boolean;
  onClose: () => void;
  onRefetch: () => void;
}

// PipelineCardDetails is the right-hand details panel opened by clicking a
// pipeline card. It docks beside the board (non-modal — the board stays fully
// interactive, and clicking another card just swaps the panel's contents) and
// projects the selected card's inputs, produced elements, and (when the
// pipeline is blocked on a human gate) the response form.
export default function PipelineCardDetails({
  card,
  stale,
  onClose,
  onRefetch,
}: Props) {
  return (
    <aside
      className="flex w-[26rem] max-w-[80vw] shrink-0 flex-col border-l border-border-default bg-surface-1"
      aria-label={`Details for ${card.title}`}
    >
      <div className="flex items-start justify-between gap-2 border-b border-border-default px-4 py-3">
        <div className="min-w-0">
          <h2 className="truncate text-sm font-semibold text-fg-default" title={card.title}>
            {card.title}
          </h2>
          <p className="mt-0.5 truncate text-xs text-fg-subtle">
            {card.kind === "task" ? "Task" : "Run"}
            {card.bot_id ? ` · ${card.bot_id}` : ""}
          </p>
        </div>
        <IconButton label="Close details" size="sm" variant="ghost" onClick={onClose}>
          <Cross2Icon />
        </IconButton>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        <PipelineCardDetailsBody card={card} stale={stale} onRefetch={onRefetch} />
      </div>
    </aside>
  );
}
