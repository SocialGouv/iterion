import {
  ArrowLeftIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  Cross2Icon,
  ExternalLinkIcon,
} from "@radix-ui/react-icons";
import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { Link } from "wouter";

import { pipelineBoardImageURL, type PipelineBoardCard } from "@/api/pipelineBoards";
import type { UnifiedStatus } from "@/components/Runs/runStatusClasses";
import { Badge, Button, IconButton, InlineBanner, StatusBadge } from "@/components/ui";

import { cardRoutePath } from "./cardRoute";
import { ImagePreviewDialog } from "./ImagePreview";
import { ProducedElements } from "./ProducedElements";
import { SequentialReviews } from "./SequentialReviews";

// Overlay drawer width is an explicit persisted pixel width with a drag
// handle on its left edge (same pattern as the run console's LeftPanel).
const DRAWER_WIDTH_KEY = "pipeline-card-details.width";
const DRAWER_WIDTH_DEFAULT = 448; // 28rem, the historical fixed width
const DRAWER_WIDTH_MIN = 320;
const DRAWER_WIDTH_MAX = 960;

// clampDrawerWidth keeps a persisted/dragged width inside sane bounds so a
// corrupted value can't strand the drawer off-screen or hair-thin.
function clampDrawerWidth(w: number): number {
  if (!Number.isFinite(w)) return DRAWER_WIDTH_DEFAULT;
  return Math.min(DRAWER_WIDTH_MAX, Math.max(DRAWER_WIDTH_MIN, w));
}

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
      <ImagePreviewDialog
        open={viewerOpen}
        onOpenChange={setViewerOpen}
        src={url}
        alt={path}
        title={<span className="font-mono text-xs">{path}</span>}
        description={
          paths.length > 1 ? (
            <span>
              Image {current + 1}/{paths.length}
            </span>
          ) : undefined
        }
        downloadHref={url}
        footerExtra={
          paths.length > 1 ? (
            <div className="flex items-center gap-2">
              {cycler(-1, "md")}
              <span className="font-mono text-xs text-fg-muted">
                {current + 1}/{paths.length}
              </span>
              {cycler(1, "md")}
            </div>
          ) : undefined
        }
      />
    </dd>
  );
}

// Contract keys the multi-pipeline bus surfaces first (matches native.ContractDisplayKeys).
const CONTRACT_KEYS = [
  "input_path",
  "asset_id",
  "feature_id",
  "family_id",
  "pipeline_kind",
  "produces",
  "consumes",
  "revision_id",
  "request_hash",
  "doc_refs",
  "auto_ready",
  "require_blocker_labels",
] as const;

const CONTRACT_KEY_SET = new Set<string>(CONTRACT_KEYS);
// InputsList renders the pipeline's full entry input (launch vars / task
// bot-args) as an untruncated key → value list. The sidebar has the room the
// compact card body does not, so values wrap instead of clipping. Values that
// are image paths (reference-image lists…) render as an inline carousel
// instead of bare paths. Contract keys are shown separately above.
function InputsList({ input }: { input?: Record<string, unknown> }) {
  const entries = input
    ? Object.entries(input).filter(([k]) => !CONTRACT_KEY_SET.has(k))
    : [];
  if (entries.length === 0) {
    return <p className="text-xs italic text-fg-subtle">No additional inputs.</p>;
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
          title={`Priority ${card.priority} — higher numbers launch first once ready`}
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

// ContractSection shows stable bot_args (request file + correlation) above
// the free-form Inputs dump so operators don't hunt opaque key blobs.
function ContractSection({ card }: { card: PipelineBoardCard }) {
  const input = card.entry_input ?? {};
  const rows: { key: string; value: string }[] = [];
  for (const key of CONTRACT_KEYS) {
    const raw = input[key];
    if (raw === undefined || raw === null || raw === "") continue;
    const value = stringifyValue(raw).trim();
    if (!value) continue;
    rows.push({ key, value });
  }
  if (rows.length === 0) return null;
  return (
    <section aria-label="Ticket contract" className="space-y-2">
      <SectionHeading>Ticket contract</SectionHeading>
      <dl className="space-y-1.5">
        {rows.map(({ key, value }) => (
          <div key={key} className="space-y-0.5">
            <dt className="text-micro font-medium uppercase tracking-wide text-fg-muted">
              {key}
            </dt>
            <dd className="break-all rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1 font-mono text-xs text-fg-default">
              {value}
            </dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

// SpawnTreeSection shows planner provenance: who created this ticket and
// which children it published. Distinct from hard blockers (scheduling).
function SpawnTreeSection({ card }: { card: PipelineBoardCard }) {
  const children = card.children ?? [];
  const hasParent = !!card.parent_issue_id;
  if (!hasParent && children.length === 0) return null;
  return (
    <section aria-label="Plan tree" className="space-y-3">
      {hasParent && (
        <div className="space-y-1.5">
          <SectionHeading>Spawned from</SectionHeading>
          <div className="rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1.5 text-xs">
            <div className="font-medium text-fg-default">
              {card.parent_title || card.parent_issue_id}
            </div>
            <div className="mt-0.5 font-mono text-micro text-fg-subtle">
              {card.parent_issue_id}
            </div>
          </div>
        </div>
      )}
      {children.length > 0 && (
        <div className="space-y-1.5">
          <SectionHeading>
            Children
            {card.children_summary
              ? ` (${card.children_summary.total})`
              : ` (${children.length})`}
          </SectionHeading>
          {card.children_summary && (
            <p className="text-micro text-fg-muted">
              <span className="font-medium tabular-nums text-fg-default">
                {(card.children_summary.done ?? 0) +
                  (card.children_summary.failed ?? 0)}
                /{card.children_summary.total} closed
              </span>
              {" · "}
              {card.children_summary.ready} ready ·{" "}
              {card.children_summary.in_progress} live ·{" "}
              {card.children_summary.open} open
            </p>
          )}
          <ul className="space-y-1">
            {children.map((ch) => (
              <li
                key={ch.issue_id}
                className="flex flex-wrap items-center gap-1.5 rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1 text-xs"
              >
                <span className="min-w-0 flex-1 truncate font-medium text-fg-default">
                  {ch.title || ch.issue_id}
                </span>
                {ch.state && (
                  <Badge variant="neutral" title={`State: ${ch.state}`}>
                    {ch.state}
                  </Badge>
                )}
                {ch.bot_id && (
                  <span className="truncate text-micro text-fg-subtle">
                    {ch.bot_id}
                  </span>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  );
}

// DependenciesSection surfaces hard deps (blockers) and reverse deps
// (blocking). Distinct from human-review "Blocked" — these are ticket-to-
// ticket gates the launch loop honours.
function DependenciesSection({ card }: { card: PipelineBoardCard }) {
  const blockers = card.blockers ?? [];
  const blocking = card.blocking ?? [];
  if (blockers.length === 0 && blocking.length === 0) return null;
  return (
    <section aria-label="Dependencies" className="space-y-3">
      {blockers.length > 0 && (
        <div className="space-y-1.5">
          <SectionHeading>
            Depends on
            {(card.open_blocker_count ?? 0) > 0
              ? ` (${card.open_blocker_count} open)`
              : ""}
          </SectionHeading>
          <ul className="space-y-1">
            {blockers.map((b) => (
              <li
                key={b.id}
                className="flex flex-wrap items-center gap-1.5 rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1 text-xs"
              >
                <span className="min-w-0 flex-1 truncate font-medium text-fg-default">
                  {b.title || b.id}
                </span>
                {b.state && (
                  <Badge
                    variant={b.satisfied ? "success" : "warning"}
                    title={b.satisfied ? "Hard dep satisfied (done)" : `State: ${b.state}`}
                  >
                    {b.state}
                  </Badge>
                )}
                {!b.satisfied && !b.state && (
                  <Badge variant="danger" title="Blocker issue not found">
                    missing
                  </Badge>
                )}
                {b.missing_labels && b.missing_labels.length > 0 && (
                  <Badge
                    variant="warning"
                    title={`Needs labels: ${b.missing_labels.join(", ")}`}
                  >
                    needs {b.missing_labels.join(", ")}
                  </Badge>
                )}
                <code className="text-micro text-fg-subtle" title={b.id}>
                  {b.id.slice(0, 16)}
                </code>
              </li>
            ))}
          </ul>
        </div>
      )}
      {blocking.length > 0 && (
        <div className="space-y-1.5">
          <SectionHeading>Blocks</SectionHeading>
          <ul className="space-y-1">
            {blocking.map((b) => (
              <li
                key={b.id}
                className="flex flex-wrap items-center gap-1.5 rounded-md border border-border-subtle bg-surface-2/40 px-2 py-1 text-xs"
              >
                <span className="min-w-0 flex-1 truncate font-medium text-fg-default">
                  {b.title || b.id}
                </span>
                <code className="text-micro text-fg-subtle" title={b.id}>
                  {b.id.slice(0, 16)}
                </code>
              </li>
            ))}
          </ul>
        </div>
      )}
      {card.launch_blocked_reason && (
        <p className="text-caption text-fg-muted">
          Launch gate:{" "}
          <code className="text-fg-default">{card.launch_blocked_reason}</code>
        </p>
      )}
    </section>
  );
}

// PipelineCardDetailsBody composes the sidebar sections by lane:
//   - Todo (no run started)              → Inputs only.
//   - In progress                        → Inputs + Produced elements.
//   - In progress, awaiting human input  → + the response form (first, so the
//                                          operator's action is front and centre).
//   - Closed, success                    → Inputs + Result + Produced elements.
//   - Closed, failed                     → Failure reason + Inputs + Produced
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
    (card.column_id === "in_progress" || card.column_id === "closed");
  // A Closed card is a failure when the run failed/was cancelled; otherwise it
  // finished successfully (drives the Failure vs Result sections below).
  const closedFailed = card.column_id === "closed" && card.failed === true;
  const closedSuccess = card.column_id === "closed" && card.failed !== true;
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

      <ContractSection card={card} />

      <div id="pipeline-card-deps">
        <DependenciesSection card={card} />
      </div>

      <SpawnTreeSection card={card} />

      {reviews.length > 0 && (
        <section
          id="pipeline-card-review"
          aria-label="Response required"
          className="space-y-2"
        >
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

      {closedFailed && card.error && (
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

      {closedSuccess && card.output && (
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
  /** "overlay" (default): fixed panel over the board. "page": full-page shell. */
  presentation?: "overlay" | "page";
  /** Scroll/highlight a section when the drawer opens. */
  focusSection?: "default" | "deps" | "review";
}

// PipelineCardDetails projects a card's contract, deps, inputs, produced
// elements, and human reviews. Overlay mode floats above the board (board
// stays full width); page mode is the GitHub-style dedicated route.
export default function PipelineCardDetails({
  card,
  stale,
  onClose,
  onRefetch,
  presentation = "overlay",
  focusSection = "default",
}: Props) {
  const fullPath = cardRoutePath(card);
  // Scrim ignores the first tick so the same click that opened the drawer
  // cannot immediately close it (portal mounts under the cursor).
  const [scrimArmed, setScrimArmed] = useState(false);

  useEffect(() => {
    if (presentation !== "overlay") return;
    setScrimArmed(false);
    const t = window.setTimeout(() => setScrimArmed(true), 0);
    return () => window.clearTimeout(t);
  }, [presentation, card.id]);

  useEffect(() => {
    if (focusSection === "default") return;
    const id =
      focusSection === "deps"
        ? "pipeline-card-deps"
        : focusSection === "review"
          ? "pipeline-card-review"
          : null;
    if (!id) return;
    const t = window.setTimeout(() => {
      document.getElementById(id)?.scrollIntoView({ behavior: "smooth", block: "start" });
    }, 50);
    return () => window.clearTimeout(t);
  }, [focusSection, card.id]);

  // Escape closes the overlay (page mode uses browser back / explicit link).
  useEffect(() => {
    if (presentation !== "overlay") return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [presentation, onClose]);

  // Lock body scroll while the overlay is up (board can still reflow underneath).
  useEffect(() => {
    if (presentation !== "overlay") return;
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = prev;
    };
  }, [presentation]);

  const header =
    presentation === "page" ? (
      // Full-page: breadcrumb-style back (not a popup close).
      <div className="flex shrink-0 flex-wrap items-start justify-between gap-3 border-b border-border-default px-6 py-4">
        <div className="min-w-0 flex-1 space-y-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={onClose}
            leadingIcon={<ArrowLeftIcon />}
            className="-ml-2"
          >
            Back to pipelines
          </Button>
          <div className="min-w-0">
            <h1 className="text-lg font-semibold text-fg-default" title={card.title}>
              {card.title}
            </h1>
            <p className="mt-0.5 truncate text-xs text-fg-subtle">
              {card.kind === "task" ? "Task" : "Run"}
              {card.bot_id ? ` · ${card.bot_id}` : ""}
            </p>
          </div>
        </div>
      </div>
    ) : (
      <div className="flex shrink-0 items-start justify-between gap-2 border-b border-border-default px-4 py-3">
        <div className="min-w-0">
          <h2 className="truncate text-sm font-semibold text-fg-default" title={card.title}>
            {card.title}
          </h2>
          <p className="mt-0.5 truncate text-xs text-fg-subtle">
            {card.kind === "task" ? "Task" : "Run"}
            {card.bot_id ? ` · ${card.bot_id}` : ""}
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-0.5">
          <Link
            href={fullPath}
            className="inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-subtle hover:bg-surface-2 hover:text-fg-default"
            title="Open full page"
            aria-label="Open full page"
          >
            <ExternalLinkIcon />
          </Link>
          <IconButton label="Close details" size="sm" variant="ghost" onClick={onClose}>
            <Cross2Icon />
          </IconButton>
        </div>
      </div>
    );

  const body = (
    <div
      className={`min-h-0 flex-1 overflow-y-auto py-3 ${
        presentation === "page" ? "px-6" : "px-4"
      }`}
    >
      <PipelineCardDetailsBody card={card} stale={stale} onRefetch={onRefetch} />
    </div>
  );

  if (presentation === "page") {
    return (
      <div
        className="flex h-full min-h-0 w-full flex-col"
        aria-label={`Details for ${card.title}`}
      >
        {header}
        {body}
      </div>
    );
  }

  // Portal to body: the board tree is under main overflow:hidden (and card
  // hover uses transform), which traps position:fixed descendants and made
  // the drawer invisible / unusable. Same pattern as Dialog/Drawer.
  return createPortal(
    <>
      <div
        role="presentation"
        className="fixed inset-0 bg-scrim-modal animate-fade-in-opacity"
        style={{ zIndex: "var(--z-overlay)" }}
        aria-hidden={!scrimArmed}
        onClick={scrimArmed ? onClose : undefined}
      />
      <aside
        className="fixed inset-y-0 right-0 flex w-[min(28rem,100vw)] flex-col border-l border-border-default bg-surface-1 shadow-[var(--shadow-lg)] animate-slide-in-right"
        style={{ zIndex: "var(--z-modal)" }}
        aria-label={`Details for ${card.title}`}
        role="dialog"
        aria-modal="true"
      >
        {header}
        {body}
      </aside>
    </>,
    document.body,
  );
}
