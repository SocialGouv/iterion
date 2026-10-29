import { useState } from "react";
import { GitBranch } from "lucide-react";

import { Badge } from "@/components/ui/Badge";
import { formatRelative } from "@/lib/format";
import { runOutcomeFailed } from "@/components/shared/LastRunOutcome";
import type { DispatchSkipView, RetryView, RunningView } from "@/api/dispatcher";
import type { NativeIssue } from "@/api/native";

import { ApproveTriageBanner } from "./ApproveTriage";
import { labelPalette, pickPinnedFields, shortID } from "./labelPalette";
import { PushToForgeButton } from "./PushToForge";

// Max label chips shown on a card before collapsing the rest into "+N".
const MAX_CARD_LABELS = 3;

// TERMINAL_BOARD_STATES lists the native-tracker state names treated as
// "no more work" for UI purposes. The runtime contract is that any
// state with `terminal: true` in the board config qualifies — but the
// card doesn't carry the board's flag here, so we hard-code the
// canonical names. Keep in sync with the defaults in
// pkg/dispatcher/native/board.go's NewStore (done + blocked + cancelled).
const TERMINAL_BOARD_STATES = new Set(["done", "blocked", "cancelled"]);

interface IssueCardProps {
  iss: NativeIssue;
  selected: boolean;
  running?: RunningView;
  retrying?: RetryView;
  // skip: present when the dispatcher refused to claim this eligible
  // issue because its explicit `bot` is unresolvable / unrouteable.
  // Rendered as a warning badge so the stall is visible + actionable.
  skip?: DispatchSkipView;
  // activeLabels: the set of labels currently in the board-level
  // filter, so each card's label chip can show its active state and
  // operators can see which chips already filter the view.
  activeLabels: Set<string>;
  // onClick receives the mouse event so the parent can update the
  // selection (plain click = select; Shift / Ctrl / Meta = multi-select).
  onClick: (e: React.MouseEvent) => void;
  // onOpen opens the issue modal — triggered by a double-click on the
  // card or a plain click on the title text (GitHub-style).
  onOpen: () => void;
  // onDragStart receives the drag event so the parent can decide
  // whether to drag this card alone or the whole multi-selection
  // and write the right payload into dataTransfer.
  onDragStart: (e: React.DragEvent) => void;
  onLabelClick: (label: string) => void;
  onCancelRun: () => void;
  onOpenRun: (runId: string) => void;
  onShowRetryDetails: () => void;
}

export function IssueCard({
  iss,
  selected,
  running,
  retrying,
  skip,
  activeLabels,
  onClick,
  onOpen,
  onDragStart,
  onLabelClick,
  onCancelRun,
  onOpenRun,
  onShowRetryDetails,
}: IssueCardProps) {
  // Hover preview: synthesise a multi-line title combining body
  // (truncated) + key fields + blocker count so the OS-native tooltip
  // provides a quick peek without forcing a modal open. Title strings
  // render with newlines on all major browsers.
  const previewLines: string[] = [];
  if (iss.body) {
    const trimmed = iss.body.trim();
    previewLines.push(trimmed.length > 240 ? trimmed.slice(0, 237) + "…" : trimmed);
  }
  if (iss.fields && Object.keys(iss.fields).length > 0) {
    previewLines.push(
      Object.entries(iss.fields)
        .map(([k, v]) => `${k}: ${String(v)}`)
        .join("\n"),
    );
  }
  if (iss.blockers && iss.blockers.length > 0) {
    previewLines.push(`Blocked by: ${iss.blockers.join(", ")}`);
  }
  const hoverTitle = previewLines.length > 0 ? previewLines.join("\n\n") : undefined;
  const [dragging, setDragging] = useState(false);
  const pinnedFields = iss.fields ? pickPinnedFields(iss.fields) : [];
  // The card carries no widget role: it holds the label chips, the run
  // links and the banner buttons, so a role="button" around them is
  // `nested-interactive` (WCAG 4.1.2) — and a role whose Enter opened the
  // modal while a click selected the card made the two paths disagree.
  // The click selects (modified: multi-select), the double-click opens,
  // and the title button below is the keyboard path to the modal.
  return (
    <div
      draggable
      data-issue-card
      title={hoverTitle}
      onDragStart={(e) => {
        onDragStart(e);
        setDragging(true);
      }}
      onDragEnd={() => setDragging(false)}
      onClick={onClick}
      onDoubleClick={onOpen}
      className={`bg-surface-0 border rounded-[var(--radius-md)] p-2 text-sm cursor-grab active:cursor-grabbing shadow-[var(--shadow-sm)] transition-[transform,box-shadow,border-color] duration-[var(--motion-fast)] ease-[var(--motion-ease)] ${
        dragging ? "scale-[1.02] shadow-[var(--shadow-lg)]" : "hover:-translate-y-0.5 hover:shadow-[var(--shadow-md)]"
      } ${
        selected
          ? "border-accent ring-1 ring-accent/40"
          : "border-border-default hover:border-accent/40"
      }`}
    >
      <div className="flex items-start gap-2">
        <button
          type="button"
          // GitHub-style: the title text is the affordance that opens the
          // modal — a real control, so Enter and Space reach the modal too
          // and the card itself needs no role. A plain click opens; a
          // modified click falls through to the card's selection handler
          // for multi-select. Its accessible name is the title it renders,
          // which is also the card's only unique text.
          className="text-fg-default flex-1 text-left cursor-pointer hover:underline focus-visible:underline"
          onClick={(e) => {
            if (e.ctrlKey || e.metaKey || e.shiftKey) return;
            e.stopPropagation();
            onOpen();
          }}
        >
          {iss.title}
        </button>
        {iss.priority && iss.priority > 0 ? (
          <Badge
            variant="warning"
            size="sm"
            title={`Priority ${iss.priority} — higher numbers sort first`}
          >
            P{iss.priority}
          </Badge>
        ) : null}
      </div>
      {pinnedFields.length > 0 && (
        <div className="mt-0.5 flex items-center gap-2 text-caption text-fg-subtle flex-wrap">
          {pinnedFields.map(([k, v]) => (
            <span key={k} className="flex items-center gap-1">
              <span className="font-mono">{k}:</span>
              <span className="text-fg-default">{String(v)}</span>
            </span>
          ))}
        </div>
      )}
      {iss.labels && iss.labels.length > 0 && (
        <div className="mt-1 flex flex-wrap gap-1">
          {iss.labels.slice(0, MAX_CARD_LABELS).map((l) => {
            const palette = labelPalette(l);
            const active = activeLabels.has(l);
            return (
              <button
                key={l}
                type="button"
                // Stop propagation so a chip click only toggles the
                // board's label filter — without this the card's
                // onClick would also open the issue modal, which is
                // not what the operator asked for.
                onClick={(e) => {
                  e.stopPropagation();
                  onLabelClick(l);
                }}
                className={`text-caption px-1.5 py-0.5 rounded hover:ring-1 hover:ring-accent transition ${
                  active ? "ring-1 ring-accent" : ""
                }`}
                style={palette}
                title={
                  active
                    ? `Click to remove ${l} from the board filter`
                    : `Click to filter board by ${l}`
                }
              >
                {l}
              </button>
            );
          })}
          {iss.labels.length > MAX_CARD_LABELS && (
            <Badge
              variant="neutral"
              size="sm"
              title={iss.labels.slice(MAX_CARD_LABELS).join(", ")}
            >
              +{iss.labels.length - MAX_CARD_LABELS}
            </Badge>
          )}
        </div>
      )}
      <div className="mt-1 flex items-center gap-2 text-caption text-fg-muted flex-wrap">
        <code className="text-fg-subtle">{shortID(iss.id)}</code>
        {iss.bot && (
          <Badge
            variant="accent"
            size="sm"
            className="font-mono"
            title={`Will dispatch via ${iss.bot} (overrides dispatcher config)`}
          >
            🤖 {iss.bot}
          </Badge>
        )}
        {iss.assignee && <span>@{iss.assignee}</span>}
        {iss.claim && (
          <span
            className="text-warning-fg"
            title={`Locked by ${iss.claim} — the dispatcher holds the claim until the run finishes.`}
          >
            claimed by {iss.claim}
          </span>
        )}
        {iss.last_run_id && !running && (() => {
          const lastRunId = iss.last_run_id;
          // "Live" derives from the card's own board state — the local
          // dispatcher's running view (which has its own chip below) is
          // absent on the cloud board, where in_progress is the signal.
          const live = iss.state === "in_progress";
          // The run's RESULT, not just the pointer to it (#1501). A card
          // whose run died otherwise reads exactly like one that was
          // dispatched and worked — on the grid, where the operator looks
          // first. The detail (code, message) rides the title: the card has
          // room for a word, the modal renders the rest.
          const status = iss.last_run_status?.trim();
          const failed = !live && runOutcomeFailed(iss);
          const outcome = !live && status ? ` · ${status}` : "";
          const detail = [iss.last_run_error_code, iss.last_run_error].filter(Boolean).join(" — ");
          return (
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onOpenRun(lastRunId);
              }}
              className={`font-mono hover:underline ${failed ? "text-warning-fg" : "text-info"}`}
              title={
                live
                  ? `Open the live run on this issue (run ${lastRunId})`
                  : `Open the last run on this issue (run ${lastRunId})${status ? ` — ${status}` : ""}${detail ? `: ${detail}` : ""}`
              }
            >
              {live ? "▶ live run" : `↪ last run${outcome}`}
            </button>
          );
        })()}
        {iss.awaiting_input && !running && (
          <Badge
            variant="warning"
            size="sm"
            title="This issue's most recent run is paused waiting for input — open the card to answer and resume. (Denormalized hint; verified against the run when you open it.)"
          >
            ⏸ Awaiting input
          </Badge>
        )}
        {iss.external?.repo && (
          <Badge
            variant="neutral"
            size="sm"
            className="max-w-40 overflow-hidden"
            leadingIcon={
              <GitBranch className="h-3 w-3 shrink-0 opacity-70" aria-hidden="true" />
            }
            title={`${iss.external.provider} · ${iss.external.repo}${
              iss.external.number ? ` #${iss.external.number}` : ""
            }${iss.external.author ? ` · opened by @${iss.external.author}` : ""}`}
          >
            <span className="truncate">{iss.external.repo}</span>
          </Badge>
        )}
        {iss.updated_at && (
          <span className="text-fg-subtle" title={iss.updated_at}>
            · updated {formatRelative(iss.updated_at)}
          </span>
        )}
        <PushToForgeButton iss={iss} />
      </div>
      <ApproveTriageBanner iss={iss} compact />
      {running && (
        <div className="mt-1 flex items-center justify-between gap-2 rounded bg-success-soft px-1.5 py-1 text-caption text-success-fg">
          <button
            type="button"
            onClick={(e) => {
              e.stopPropagation();
              onOpenRun(running.run_id);
            }}
            className="text-left flex-1 hover:underline cursor-pointer"
            title={
              running.attempt && running.attempt > 0
                ? `Open run ${running.run_id} (resume of a prior failed_resumable run — attempt ${running.attempt + 1})`
                : `Open run ${running.run_id}`
            }
          >
            ● {running.attempt && running.attempt > 0 ? "resuming" : "running"}
            {running.attempt && running.attempt > 0 ? (
              <span className="ml-1 text-warning-fg">#{running.attempt + 1}</span>
            ) : null}
            {running.last_event_name && (
              <span className="ml-1 text-success-fg">— {running.last_event_name}</span>
            )}
          </button>
          <button
            type="button"
            className="rounded border border-success/40 px-1.5 py-0.5 text-caption hover:bg-success-soft"
            onClick={(e) => {
              e.stopPropagation();
              onCancelRun();
            }}
            title="Cancel this in-flight run"
          >
            cancel
          </button>
        </div>
      )}
      {!running && retrying && !TERMINAL_BOARD_STATES.has(iss.state) && (
        <button
          type="button"
          className="mt-1 w-full text-left rounded bg-warning-soft px-1.5 py-1 text-caption text-warning-fg cursor-pointer hover:bg-warning-soft"
          onClick={(e) => {
            e.stopPropagation();
            onShowRetryDetails();
          }}
          title={retrying.error ? `Last error: ${retrying.error}` : undefined}
        >
          ⏳ retrying (attempt {retrying.attempt})
          {retrying.error && (
            <span className="ml-1 text-warning-fg truncate">— {retrying.error}</span>
          )}
        </button>
      )}
      {!running && retrying && TERMINAL_BOARD_STATES.has(iss.state) && (
        <div
          className="mt-1 rounded bg-fg-muted/10 px-1.5 py-1 text-caption text-fg-subtle"
          title={`The dispatcher still has a retry entry for this issue, but it's in a terminal state (${iss.state}) — the retry will be skipped on the next tick.`}
        >
          stale retry queued — will be skipped (issue in {iss.state})
        </div>
      )}
      {!running && skip && (
        <button
          type="button"
          className="mt-1 w-full text-left rounded bg-danger-soft px-1.5 py-1 text-caption text-danger-fg cursor-pointer hover:bg-danger-soft"
          onClick={(e) => {
            e.stopPropagation();
            onOpen();
          }}
          title={`The dispatcher refuses to run this issue: ${skip.reason}. Fix the bot in the issue editor or add it to assignee_workflows.`}
        >
          ⚠ won&apos;t dispatch
          <span className="ml-1 text-danger-fg truncate">— {skip.reason}</span>
        </button>
      )}
    </div>
  );
}
