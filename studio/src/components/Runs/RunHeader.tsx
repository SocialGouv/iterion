import { errorMessage } from "@/lib/errorHints";
import { downloadBlob } from "@/lib/download";
import { useState } from "react";
import { useLocation } from "wouter";
import { ClockIcon, LockClosedIcon, Pencil1Icon, ResetIcon } from "@radix-ui/react-icons";

import type { RunHeader as RunHeaderType } from "@/api/runs";
import { cancelRun, deleteRun, getRun, loadEvents, pauseRun, renameRun } from "@/api/runs";
import {
  Button,
  CopyButton,
  IconButton,
  InlineBanner,
  LiveDot,
  StatusBadge,
  Tooltip,
} from "@/components/ui";
import WSStatusDot from "@/components/shared/WSStatusDot";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";
import { formatDateTime, formatRelative } from "@/lib/format";
import { useRunStore, type WsState } from "@/store/run";
import { useServerInfoStore } from "@/store/serverInfo";

import ForkDialog from "./ForkDialog";
import ResumeDialog from "./ResumeDialog";
import { RunShellPanel } from "./RunShellPanel";
import BackendsUsedRow from "./runHeader/BackendsUsedRow";
import BotChip from "./runHeader/BotChip";
import DeploymentRow from "./runHeader/DeploymentRow";
import ErrorHintRow from "./runHeader/ErrorHintRow";
import FinalizationRow from "./runHeader/FinalizationRow";
import ForkedFromRow from "./runHeader/ForkedFromRow";
import RunChildrenPanel from "./runHeader/RunChildrenPanel";
import NotesRow from "./runHeader/NotesRow";
import ResultLinksRow from "./runHeader/ResultLinksRow";
import RunInformationAccordion from "./runHeader/RunInformationAccordion";
import RunNameEditor from "./runHeader/RunNameEditor";
import RunTagsRow from "./runHeader/RunTagsRow";
import SourceTicketRow from "./runHeader/SourceTicketRow";
import WSDisconnectBanner from "./runHeader/WSDisconnectBanner";
import { cancelTooltip } from "./runHeader/cancelTooltip";
import { isDeletable, isResumable } from "./runStatusActions";

interface Props {
  run: RunHeaderType;
  active: boolean;
  wsState: WsState;
  // Restores the run-console panels to their default layout.
  onResetLayout?: () => void;
  // When true, drop the component's own bottom border so the parent can
  // fuse the header rows with the metrics ribbon into one bordered block.
  bare?: boolean;
}

export default function RunHeader({ run, active, wsState, onResetLayout, bare = false }: Props) {
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  // Headline result-links (opened PR, live deploy) derived from the run's
  // preview_url events — rebuilt on reload from the replayed event log.
  const resultLinks = useRunStore((s) => s.resultLinks);
  const { busy, error, run: runAction, setError } = useAsyncAction();
  const [resumeOpen, setResumeOpen] = useState(false);
  const [forkOpen, setForkOpen] = useState(false);
  const [shellOpen, setShellOpen] = useState(false);
  const [editingName, setEditingName] = useState(false);
  const [, setLocation] = useLocation();
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");

  // canCancel covers every state where a cancel actually does something
  // distinct from Resume:
  //   - running:               abort an in-flight execution (local mode
  //                            additionally requires the engine to be
  //                            "active" in this server's process).
  //   - paused_waiting_human:  user gives up on the workflow without
  //                            answering — engine drops the goroutine
  //                            and the run terminates.
  //   - queued:                cloud-mode run on the NATS queue;
  //                            cancel removes the message before any
  //                            runner picks it up.
  // `failed_resumable` is intentionally excluded — no in-flight work to
  // abort, and the Resume button already handles the common case. The
  // rare "give up on a failed run" path is Resume → cancel-from-pause.
  // The "active" flag is only meaningful for in-process running runs
  // (local mode); cloud and paused/queued runs are never "active" in
  // this server's process. See cloud-ready plan §F (T-14).
  const canCancel =
    (run.status === "running" && active) ||
    run.status === "paused_waiting_human" ||
    run.status === "paused_operator" ||
    run.status === "queued";
  // Soft pause is only meaningful while the run is actually executing
  // in this process — paused/queued/terminal states have nothing to
  // interrupt. The "active" gate also keeps the button hidden for
  // cloud-mode runs whose engine lives in another process (Phase 1
  // doesn't ship cross-process pause; cancel falls back via NATS but
  // pause has no NATS subject yet).
  const canPause = run.status === "running" && active;
  // The WS "disconnected — data may be stale" banner is only meaningful
  // while MORE live events are expected (running / queued). A paused or
  // terminal run legitimately ENDS its event stream — the broker closes
  // the channel on pause/completion and the WS emits `terminated`, which
  // the client turns into wsState="closed" — so the data is complete, not
  // stale. Without this gate a paused run (waiting for the operator's
  // answer) shows a false "disconnected" alarm whose Reconnect re-triggers
  // the same `terminated` immediately. Resume redials the WS on its own
  // (wsReconnectToken), so the live tail returns when the run continues.
  const liveStreamExpected =
    run.status === "running" || run.status === "queued";
  // Fork is offered for every run that has a checkpoint to anchor on
  // — paused, finished, failed, cancelled. We don't gate by status
  // because "fork from a finished run" is a perfectly valid use case
  // (re-run the last LLM turn with different inputs).
  const checkpointNode = run.checkpoint?.node_id ?? null;
  const canFork = Boolean(checkpointNode);
  // Resume from header is a "best-effort" trigger — for paused_waiting_human
  // runs the user normally fills the Pause form in the detail panel
  // (Phase 5). The header button stays for failed_resumable / cancelled
  // / paused_operator runs (isResumable — shared with the run list's
  // inline Resume quick action).
  const canResume = isResumable(run.status);

  const onCancel = () => runAction(() => cancelRun(run.id));

  // onPause posts to /api/runs/:id/pause. The engine flips the
  // persisted status to paused_operator at the next safe boundary; the
  // WS run_paused event then drives the UI update. We don't optimistically
  // flip the local store — the boundary is cooperative and the server
  // is authoritative.
  const onPause = () => runAction(() => pauseRun(run.id));

  // Export the run as a single JSON document bundling the snapshot
  // (run header + executions) and the full events stream. Useful for
  // sharing reproductions, attaching to bug reports, or post-hoc
  // analysis in notebooks. We assemble client-side to avoid a new
  // backend endpoint and to inherit the existing /events pagination.
  const onExport = async () => {
    try {
      const [snap, events] = await Promise.all([getRun(run.id), loadEvents(run.id)]);
      const payload = JSON.stringify(
        { snapshot: snap, events, exported_at: new Date().toISOString() },
        null,
        2,
      );
      const blob = new Blob([payload], { type: "application/json" });
      downloadBlob(blob, `run-${run.id}.json`);
    } catch (e) {
      setError(errorMessage(e));
    }
  };

  const showFinalization = Boolean(run.final_commit);

  // Post-mortem shell: local mode, run at rest, preserved worktree on
  // disk (worktree_available mirrors the server-side gate, so the
  // button and the endpoint's 409/410 stay in lockstep).
  const runShellEnabled = useServerInfoStore(
    (s) => Boolean(s.info?.run_shell_enabled),
  );
  const shellEligible =
    runShellEnabled &&
    run.status !== "running" &&
    run.status !== "queued" &&
    Boolean(run.work_dir) &&
    run.worktree_available;

  const friendlyName = run.name || run.workflow_name;
  const startedRel = formatRelative(run.created_at);
  const finishedRel = run.finished_at ? formatRelative(run.finished_at) : null;
  const fileBase = run.file_path ? basename(run.file_path) : null;

  const onRename = async (next: string) => {
    const trimmed = next.trim();
    if (!trimmed || trimmed === friendlyName) {
      setEditingName(false);
      return;
    }
    try {
      await renameRun(run.id, trimmed);
      // Refresh the snapshot so the rest of the UI (run header,
      // tab label sync, run list) sees the new name immediately —
      // the WS stream doesn't replay snapshot pushes after a metadata
      // change, so a manual refresh is the most reliable path.
      const snap = await getRun(run.id);
      applySnapshot(snap);
    } catch (e) {
      setError(`Rename failed: ${errorMessage(e)}`);
    } finally {
      setEditingName(false);
    }
  };

  const { confirm, dialog } = useConfirm();
  const cancelWithConfirm = async () => {
    const ok = await confirm({
      title: "Cancel this run?",
      message:
        "The run stops and any in-progress work is aborted. A checkpoint is saved, so you can resume or fork it later.",
      confirmLabel: "Cancel run",
      confirmVariant: "danger",
    });
    if (ok) await onCancel();
  };

  // Delete is offered only for terminal runs — an active run must be
  // cancelled first. Irreversible: removes the run + all its data.
  // isDeletable is shared with the run list's bulk Delete.
  const canDelete = isDeletable(run.status);
  const deleteWithConfirm = async () => {
    const ok = await confirm({
      title: "Delete this run?",
      message:
        "This permanently removes the run and all its data — events, artifacts, attachments. This cannot be undone.",
      confirmLabel: "Delete run",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteRun(run.id);
      setLocation("/runs");
    } catch (e) {
      setError(`Delete failed: ${errorMessage(e)}`);
    }
  };

  return (
    <>
      <div
        className={`shrink-0 px-3 sm:px-4 py-2 flex flex-col gap-1.5 text-sm ${
          bare ? "" : "border-b border-border-default"
        }`}
      >
        {/* Row 1: friendly name + status + actions */}
        <div className="flex items-center gap-2 sm:gap-3 flex-wrap">
          {editingName ? (
            <RunNameEditor initial={friendlyName} onSubmit={onRename} onCancel={() => setEditingName(false)} />
          ) : (
            <div className="inline-flex items-center gap-1 min-w-0 group">
              <Tooltip content="Double-click to rename">
                <button
                  type="button"
                  onDoubleClick={() => setEditingName(true)}
                  className="font-semibold tracking-tight truncate max-w-md text-left hover:text-fg-default focus:outline-none focus-visible:ring-1 focus-visible:ring-accent rounded"
                >
                  <span className="truncate">{friendlyName}</span>
                </button>
              </Tooltip>
              <IconButton
                label="Rename run"
                tooltip="Rename run"
                size="sm"
                variant="ghost"
                onClick={() => setEditingName(true)}
                className="opacity-0 group-hover:opacity-100 focus:opacity-100 focus-visible:opacity-100 transition-opacity h-6 w-6"
              >
                <Pencil1Icon className="w-3 h-3" />
              </IconButton>
            </div>
          )}
          <StatusBadge status={run.status} />
          {(run.permission_mode === "ask" || run.permission_mode === "deny") && (
            <Tooltip
              content={
                run.permission_mode === "ask"
                  ? "Tool-permission gate: ask — tool calls outside the allow-list pause for your approval"
                  : "Tool-permission gate: deny — tool calls outside the allow-list are blocked"
              }
            >
              <span className="inline-flex items-center gap-1 rounded border border-warning/40 bg-warning-soft px-1.5 py-0.5 text-micro font-medium text-warning-fg">
                <LockClosedIcon className="h-3 w-3" aria-hidden />
                {run.permission_mode}
              </span>
            </Tooltip>
          )}
          {active && (
            <LiveDot
              tone="live"
              size="sm"
              label="Run is active in this server process"
            />
          )}
          <div className="ml-auto flex items-center gap-2 flex-wrap">
            <CopyButton
              value={run.id}
              label="copy run id"
              copiedLabel="run id copied"
              variant="icon"
            />
            <CopyButton
              value={typeof window === "undefined" ? "" : window.location.href}
              label="copy share link"
              copiedLabel="link copied"
              variant="share"
            />
            <Tooltip content="Download a JSON archive containing the run snapshot + every event">
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void onExport()}
              >
                Export
              </Button>
            </Tooltip>
            {onResetLayout && (
              <IconButton
                label="Reset console layout"
                tooltip="Reset console layout"
                size="sm"
                variant="ghost"
                onClick={onResetLayout}
              >
                <ResetIcon className="w-3.5 h-3.5" />
              </IconButton>
            )}
            <WSStatusDot state={wsState} />
            {canPause && (
              <Tooltip content="Pause at the next safe boundary. A checkpoint is saved; resume from this header.">
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => void onPause()}
                  disabled={busy}
                >
                  Pause
                </Button>
              </Tooltip>
            )}
            {canCancel && (
              <Tooltip content={cancelTooltip(run.status)}>
                <Button
                  variant="danger"
                  size="sm"
                  onClick={() => void cancelWithConfirm()}
                  disabled={busy}
                >
                  Cancel
                </Button>
              </Tooltip>
            )}
            {canDelete && (
              <Tooltip content="Permanently delete this run and all its data">
                <Button variant="ghost" size="sm" onClick={() => void deleteWithConfirm()}>
                  Delete
                </Button>
              </Tooltip>
            )}
            {canResume && (
              <Tooltip content="Resume from the last checkpoint.">
                <Button
                  variant="primary"
                  size="sm"
                  onClick={() => setResumeOpen(true)}
                  disabled={busy}
                >
                  Resume…
                </Button>
              </Tooltip>
            )}
            {canFork && (
              <Tooltip content="Fork: start a new run from a prior LLM turn (Shift+click forks in background).">
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => setForkOpen(true)}
                  disabled={busy}
                >
                  ⑂ Fork…
                </Button>
              </Tooltip>
            )}
          </div>
        </div>
        {/* Row 2: bot · folder · when. Each cell is muted + small so
            it stays readable but doesn't compete with the run name. */}
        <div className="flex items-center gap-3 text-micro text-fg-subtle flex-wrap">
          <BotChip run={run} fileBase={fileBase} onOpenFile={(p) =>
            setLocation(
              `/editor?file=${encodeURIComponent(p)}&from=${encodeURIComponent(run.id)}`,
            )
          } />
          {run.work_dir && !cloud && (
            <Tooltip content={run.work_dir}>
              <span className="inline-flex items-center gap-1 font-mono truncate max-w-[20rem]">
                <FolderGlyph />
                <span className="truncate">{compactDir(run.work_dir)}</span>
              </span>
            </Tooltip>
          )}
          {/* Cloud identity: the target repo slug, not the runner-pod
              path (which is neither actionable nor informative here). */}
          {cloud && run.project_path && (
            <Tooltip content={`Target repository: ${run.project_path}`}>
              <span className="inline-flex items-center gap-1 font-mono truncate max-w-[20rem]">
                <span className="truncate">{run.project_path}</span>
              </span>
            </Tooltip>
          )}
          <Tooltip content={formatDateTime(run.created_at)}>
            <span className="inline-flex items-center gap-1">
              <ClockIcon className="w-3 h-3" />
              <span>started {startedRel}</span>
              {finishedRel && <span>· finished {finishedRel}</span>}
            </span>
          </Tooltip>
          {/* run id lives on the copy-id button (row 1) + the Overview's
              Advanced details — no need to repeat the raw string here. */}
        </div>
      </div>
      {error && (
        <InlineBanner
          tone="danger"
          layout="sticky"
          dismissable
          onDismiss={() => setError(null)}
        >
          {error}
        </InlineBanner>
      )}
      {liveStreamExpected && (
        <WSDisconnectBanner state={wsState} onReconnect={requestWsReconnect} />
      )}
      <RunInformationAccordion>
        <div className="shrink-0 border-b border-border-default px-3 py-2 sm:px-4 flex flex-col gap-1.5 text-sm">
          {/* Resolved backend/model chips. Nothing renders for
              tool/compute-only runs. */}
          {run.backends_used && run.backends_used.length > 0 && (
            <BackendsUsedRow backends={run.backends_used} />
          )}
          <RunTagsRow runId={run.id} />
        </div>
        {/* Headline result-links (opened PR, live deploy) — the run's
            "what did it produce" at a glance, above the detailed
            deployment/finalization rows. */}
        <ResultLinksRow links={resultLinks} />
        {/* What the run shipped, above where its commits landed: the live
            URL is the operator's destination at the end of a deploy run. */}
        {run.deployment && <DeploymentRow deployment={run.deployment} />}
        {showFinalization && <FinalizationRow run={run} />}
        {shellEligible && (
          <div className="shrink-0 px-4 py-1.5 bg-surface-2/40 border-b border-border-default flex items-center gap-2 text-micro">
            <span className="text-fg-muted">worktree preserved</span>
            <button
              type="button"
              className="text-accent hover:underline"
              onClick={() => setShellOpen(true)}
              title="Open an interactive shell in the run's preserved worktree (local mode)"
            >
              Open post-mortem shell
            </button>
          </div>
        )}
        {run.forked_from && <ForkedFromRow run={run} />}
        <RunChildrenPanel run={run} />
        {run.source?.issue_id && <SourceTicketRow source={run.source} />}
        <ErrorHintRow run={run} onResume={() => setResumeOpen(true)} />
        <NotesRow runId={run.id} />
      </RunInformationAccordion>

      {canResume && (
        <ResumeDialog
          run={run}
          open={resumeOpen}
          onOpenChange={setResumeOpen}
        />
      )}
      {canFork && (
        <ForkDialog
          run={run}
          anchor={checkpointNode ? { nodeId: checkpointNode, turnIndex: -1 } : null}
          open={forkOpen}
          onOpenChange={setForkOpen}
        />
      )}
      {shellEligible && (
        <RunShellPanel open={shellOpen} onOpenChange={setShellOpen} runId={run.id} />
      )}
      {dialog}
    </>
  );
}

function basename(path: string): string {
  const parts = path.split(/[\\/]/);
  return parts[parts.length - 1] || path;
}

// compactDir shortens a long absolute path for the run header: the
// last two segments are usually informative enough, and the tooltip
// shows the full string for verification.
function compactDir(dir: string): string {
  const parts = dir.split(/[\\/]/).filter(Boolean);
  if (parts.length <= 2) return dir;
  return `…/${parts.slice(-2).join("/")}`;
}

function FolderGlyph() {
  return (
    <svg
      width="12"
      height="12"
      viewBox="0 0 15 15"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className="shrink-0"
      aria-hidden="true"
    >
      <path d="M1.5 4.5h4l1 1h7v6.5a.5.5 0 0 1-.5.5h-11a.5.5 0 0 1-.5-.5v-7.5z" />
    </svg>
  );
}
