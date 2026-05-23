import { useEffect, useRef, useState } from "react";
import { useLocation } from "wouter";
import { ClockIcon, FileTextIcon, OpenInNewWindowIcon, Pencil1Icon } from "@radix-ui/react-icons";

import type { RunHeader as RunHeaderType } from "@/api/runs";
import { cancelRun, getRun, loadEvents, pauseRun, renameRun } from "@/api/runs";
import { Button, CopyButton, LiveDot, StatusBadge, Tooltip } from "@/components/ui";
import WSStatusDot from "@/components/shared/WSStatusDot";
import { formatRelative } from "@/lib/format";
import { useRunStore, type WsState } from "@/store/run";

import ForkDialog from "./ForkDialog";
import ResumeDialog from "./ResumeDialog";

interface Props {
  run: RunHeaderType;
  active: boolean;
  wsState: WsState;
}

export default function RunHeader({ run, active, wsState }: Props) {
  const requestWsReconnect = useRunStore((s) => s.requestWsReconnect);
  const applySnapshot = useRunStore((s) => s.applySnapshot);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [resumeOpen, setResumeOpen] = useState(false);
  const [forkOpen, setForkOpen] = useState(false);
  const [editingName, setEditingName] = useState(false);
  const [, setLocation] = useLocation();

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
  // Fork is offered for every run that has a checkpoint to anchor on
  // — paused, finished, failed, cancelled. We don't gate by status
  // because "fork from a finished run" is a perfectly valid use case
  // (re-run the last LLM turn with different inputs).
  const checkpointNode =
    (run.checkpoint as { node_id?: string } | undefined)?.node_id ?? null;
  const canFork = Boolean(checkpointNode);
  // Resume from header is a "best-effort" trigger — for paused_waiting_human
  // runs the user normally fills the Pause form in the detail panel
  // (Phase 5). The header button stays for failed_resumable / cancelled
  // / paused_operator runs.
  const canResume =
    run.status === "failed_resumable" ||
    run.status === "cancelled" ||
    run.status === "paused_operator";

  const onCancel = async () => {
    setBusy(true);
    setError(null);
    try {
      await cancelRun(run.id);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  // onPause posts to /api/runs/:id/pause. The engine flips the
  // persisted status to paused_operator at the next safe boundary; the
  // WS run_paused event then drives the UI update. We don't optimistically
  // flip the local store — the boundary is cooperative and the server
  // is authoritative.
  const onPause = async () => {
    setBusy(true);
    setError(null);
    try {
      await pauseRun(run.id);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

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
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `run-${run.id}.json`;
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      // Defer revocation so Safari has a chance to start the download
      // (it lazily resolves the blob URL).
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  const showFinalization = Boolean(run.final_commit);

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
      setError(`Rename failed: ${(e as Error).message}`);
    } finally {
      setEditingName(false);
    }
  };

  return (
    <>
      <div className="shrink-0 border-b border-border-default px-3 sm:px-4 py-2 flex flex-col gap-1.5 text-sm">
        {/* Row 1: friendly name + status + actions */}
        <div className="flex items-center gap-2 sm:gap-3 flex-wrap">
          {editingName ? (
            <RunNameEditor initial={friendlyName} onSubmit={onRename} onCancel={() => setEditingName(false)} />
          ) : (
            <Tooltip content="Double-click to rename">
              <button
                type="button"
                onDoubleClick={() => setEditingName(true)}
                className="inline-flex items-center gap-1 font-medium truncate max-w-md text-left hover:text-fg-default group focus:outline-none"
                title={friendlyName}
              >
                <span className="truncate">{friendlyName}</span>
                <Pencil1Icon className="w-3 h-3 text-fg-subtle opacity-0 group-hover:opacity-100 transition-opacity shrink-0" />
              </button>
            </Tooltip>
          )}
          <StatusBadge status={run.status} />
          {active && (
            <LiveDot
              tone="live"
              size="sm"
              label="Run is active in this server process"
            />
          )}
          {error && (
            <span className="text-[10px] text-danger truncate max-w-xs">{error}</span>
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
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void onExport()}
              title="Download a JSON archive containing the run snapshot + every event"
            >
              Export
            </Button>
            <WSStatusDot state={wsState} />
            {canPause && (
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void onPause()}
                disabled={busy}
                title="Pause at the next safe boundary. A checkpoint is saved; resume from this header."
              >
                Pause
              </Button>
            )}
            {canCancel && (
              <Button
                variant="danger"
                size="sm"
                onClick={() => void onCancel()}
                disabled={busy}
                title={cancelTooltip(run.status)}
              >
                Cancel
              </Button>
            )}
            {canResume && (
              <Button
                variant="primary"
                size="sm"
                onClick={() => setResumeOpen(true)}
                disabled={busy}
                title="Resume from the last checkpoint."
              >
                Resume…
              </Button>
            )}
            {canFork && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setForkOpen(true)}
                disabled={busy}
                title="Fork: start a new run from a prior LLM turn (Shift+click forks in background)."
              >
                ⑂ Fork…
              </Button>
            )}
          </div>
        </div>
        {/* Row 2: bot · folder · when. Each cell is muted + small so
            it stays readable but doesn't compete with the run name. */}
        <div className="flex items-center gap-3 text-[11px] text-fg-subtle flex-wrap">
          {fileBase && (
            <Tooltip content={run.file_path!}>
              <button
                type="button"
                className="inline-flex items-center gap-1 hover:text-fg-default focus:outline-none"
                onClick={() =>
                  setLocation(
                    `/editor?file=${encodeURIComponent(run.file_path!)}&from=${encodeURIComponent(run.id)}`,
                  )
                }
                title={`Open ${run.file_path} in the editor`}
              >
                <FileTextIcon className="w-3 h-3" />
                <span className="font-mono truncate max-w-[20rem]">{fileBase}</span>
                <OpenInNewWindowIcon className="w-2.5 h-2.5 opacity-70" />
              </button>
            </Tooltip>
          )}
          {run.work_dir && (
            <Tooltip content={run.work_dir}>
              <span className="inline-flex items-center gap-1 font-mono truncate max-w-[20rem]">
                <FolderGlyph />
                <span className="truncate">{compactDir(run.work_dir)}</span>
              </span>
            </Tooltip>
          )}
          <Tooltip content={new Date(run.created_at).toLocaleString()}>
            <span className="inline-flex items-center gap-1">
              <ClockIcon className="w-3 h-3" />
              <span>started {startedRel}</span>
              {finishedRel && <span>· finished {finishedRel}</span>}
            </span>
          </Tooltip>
          <span
            className="ml-auto text-[10px] font-mono opacity-70"
            title="Run ID"
          >
            {run.id}
          </span>
        </div>
      </div>
      <WSDisconnectBanner state={wsState} onReconnect={requestWsReconnect} />
      {showFinalization && <FinalizationRow run={run} />}
      {run.forked_from && <ForkedFromRow run={run} />}
      <ErrorHintRow run={run} onResume={() => setResumeOpen(true)} />

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
    </>
  );
}

function basename(path: string): string {
  const parts = path.split(/[\\/]/);
  return parts[parts.length - 1] || path;
}

// cancelTooltip tailors the Cancel button's hover text to the run's
// status so operators understand the difference between aborting an
// in-flight run, giving up on a human gate, and dropping a queued run
// before any runner sees it.
// Exported for the unit test that locks the per-status wording.
export function cancelTooltip(status: RunHeaderType["status"]): string {
  switch (status) {
    case "queued":
      return "Drop from the queue before any runner picks this up.";
    case "paused_waiting_human":
    case "paused_operator":
      return "Cancel without answering — the run terminates.";
    case "running":
      return "Stop the run as soon as the engine reaches a safe boundary.";
    default:
      return "Cancel this run.";
  }
}

// RunNameEditor is the inline edit affordance for the run name. Mounted
// only while editing; auto-focuses and selects on mount. Enter commits,
// Escape (or blur) cancels. The id stays stable — the rename only
// updates the human-readable label.
function RunNameEditor({
  initial,
  onSubmit,
  onCancel,
}: {
  initial: string;
  onSubmit: (next: string) => void;
  onCancel: () => void;
}) {
  const [value, setValue] = useState(initial);
  const ref = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.focus();
    el.select();
  }, []);
  return (
    <input
      ref={ref}
      type="text"
      value={value}
      onChange={(e) => setValue(e.target.value)}
      onKeyDown={(e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          onSubmit(value);
        } else if (e.key === "Escape") {
          e.preventDefault();
          onCancel();
        }
      }}
      onBlur={() => onSubmit(value)}
      className="font-medium text-sm bg-surface-2 border border-accent/60 rounded px-1.5 py-0.5 min-w-[16rem] max-w-md focus:outline-none focus:border-accent"
      maxLength={200}
      aria-label="Rename run"
    />
  );
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

// 2-second debounce on the visible banner avoids flicker during normal
// reconnect blips — anything shorter than that the user shouldn't see.
function WSDisconnectBanner({
  state,
  onReconnect,
}: {
  state: WsState;
  onReconnect: () => void;
}) {
  const [showStale, setShowStale] = useState(false);
  useEffect(() => {
    if (state !== "closed") {
      setShowStale(false);
      return;
    }
    const t = window.setTimeout(() => setShowStale(true), 2000);
    return () => window.clearTimeout(t);
  }, [state]);
  if (!showStale) return null;
  return (
    <div
      role="status"
      aria-live="polite"
      aria-atomic="true"
      className="px-4 py-1.5 bg-warning-soft border-b border-warning/40 flex items-center gap-2 text-[11px] text-warning-fg"
    >
      <LiveDot tone="danger" size="sm" pulse={false} />
      <span>Live updates disconnected — data may be stale.</span>
      <Button variant="ghost" size="sm" className="ml-auto" onClick={onReconnect}>
        Reconnect
      </Button>
    </div>
  );
}

// FinalizationRow surfaces the worktree-finalization outcome (commit
// SHA, storage branch, merge target) under the main header bar so the
// user can see at a glance whether the run's commits made it back to
// their branch — and what to do if they didn't. Only rendered when
// final_commit is set (i.e. the run produced commits in its worktree).
function FinalizationRow({ run }: { run: RunHeaderType }) {
  const [copied, setCopied] = useState<string | null>(null);
  const shortSha = (run.final_commit ?? "").slice(0, 7);
  const branch = run.final_branch ?? "";
  const merged = run.merged_into ?? "";
  const status = run.merge_status;
  const strategy = run.merge_strategy;
  const mergedShort = (run.merged_commit ?? "").slice(0, 7);

  const copyTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    return () => {
      if (copyTimerRef.current != null) clearTimeout(copyTimerRef.current);
    };
  }, []);
  const copy = async (text: string, key: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(key);
      if (copyTimerRef.current != null) clearTimeout(copyTimerRef.current);
      copyTimerRef.current = setTimeout(() => {
        copyTimerRef.current = null;
        setCopied(null);
      }, 1500);
    } catch {
      // clipboard may be unavailable (insecure context) — silent
    }
  };

  return (
    <div className="shrink-0 px-4 py-1.5 bg-surface-2/40 border-b border-border-default flex items-center gap-3 text-[11px] flex-wrap">
      <span className="text-fg-muted">commit</span>
      <button
        className="font-mono text-fg-default hover:text-info"
        onClick={() => void copy(run.final_commit ?? "", "sha")}
        title="Copy full SHA"
      >
        {shortSha}
        {copied === "sha" && <span className="ml-1 text-fg-subtle">copied</span>}
      </button>
      {branch && (
        <>
          <span className="text-fg-subtle">on</span>
          <button
            className="font-mono text-fg-default hover:text-info truncate max-w-xs"
            onClick={() => void copy(branch, "branch")}
            title="Copy branch name"
          >
            {branch}
            {copied === "branch" && (
              <span className="ml-1 text-fg-subtle">copied</span>
            )}
          </button>
        </>
      )}
      <MergeStatusBadge
        status={status}
        strategy={strategy}
        merged={merged}
        mergedShort={mergedShort}
        branch={branch}
      />
    </div>
  );
}

// ForkedFromRow surfaces the parent-run breadcrumb on a forked run.
// Renders a one-line "⑂ forked from <name> @ <node>/turn <N>" with
// a click handler that focuses the parent tab (opening it if absent).
// Only mounted when run.forked_from is set.
function ForkedFromRow({ run }: { run: RunHeaderType }) {
  const [, setLocation] = useLocation();
  const parentID = run.forked_from!;
  const anchor = run.fork_anchor;
  const nodeLabel = anchor?.node_id ?? "?";
  const turnLabel = anchor?.turn_index ?? -1;
  const focusParent = () => setLocation(`/runs/${encodeURIComponent(parentID)}`);
  return (
    <div className="shrink-0 px-4 py-1.5 bg-info-soft/40 border-b border-info/30 flex items-center gap-2 text-[11px]">
      <span className="text-fg-muted">⑂ Forked from</span>
      <button
        onClick={focusParent}
        className="font-mono text-fg-default hover:text-info underline-offset-2 hover:underline"
        title="Open the parent run"
      >
        {parentID.slice(0, 12)}
      </button>
      <span className="text-fg-subtle">at</span>
      <span className="font-mono">{nodeLabel}</span>
      <span className="text-fg-subtle">/ turn</span>
      <span className="font-mono">{turnLabel}</span>
      {anchor?.rewind_code && (
        <span className="ml-1 rounded bg-warning-soft px-1 text-[10px] text-fg-default" title="Worktree was reset to the snapshot at this boundary">
          rewound
        </span>
      )}
    </div>
  );
}

// ErrorHintRow recognises common RuntimeError codes embedded in the
// `run.error` field (the engine formats them as "[CODE] message …") and
// renders a small actionable hint banner below the header. Returns null
// when the run is healthy or the error code is not recognised — we
// intentionally stay quiet rather than show a generic "Try resuming"
// hint that would dilute the targeted ones.
function ErrorHintRow({
  run,
  onResume,
}: {
  run: RunHeaderType;
  onResume: () => void;
}) {
  if (!run.error) return null;
  if (
    run.status !== "failed" &&
    run.status !== "failed_resumable" &&
    run.status !== "cancelled"
  ) {
    return null;
  }
  const code = parseErrorCode(run.error);
  const hint = errorHint(code, run);
  if (!hint) return null;
  const canResume = run.status === "failed_resumable";
  return (
    <div className="shrink-0 px-4 py-2 bg-warning-soft/40 border-b border-border-default flex items-start gap-2 text-[11px]">
      <span className="font-medium text-warning-fg shrink-0">Hint:</span>
      <span className="text-fg-default flex-1">{hint}</span>
      {canResume && (
        <Button
          variant="primary"
          size="sm"
          onClick={onResume}
          className="shrink-0"
          title="Open the Resume dialog"
        >
          Resume…
        </Button>
      )}
    </div>
  );
}

function parseErrorCode(err: string): string {
  // Matches the "[CODE] …" prefix produced by RuntimeError.Error().
  const m = err.match(/^\s*\[([A-Z_]+)\]/);
  return m ? m[1]! : "";
}

function errorHint(code: string, run: RunHeaderType): string | null {
  switch (code) {
    case "BUDGET_EXCEEDED":
      return `Raise the workflow's \`budget:\` block (max_cost_usd, max_tokens, max_iterations, or max_duration), then \`iterion resume --run-id ${run.id}${
        run.file_path ? ` --file ${run.file_path}` : ""
      } --force\` to continue past the original budget.`;
    case "RATE_LIMITED":
      return "Wait a few minutes for the provider rate limit to clear, then resume — the engine retries from the failed node.";
    case "LOOP_EXHAUSTED":
      return "Raise the loop's `(N)` count in the workflow, or accept the partial output and let the run finish.";
    case "CONTEXT_LENGTH_EXCEEDED":
      return "Lower the per-node compaction `ratio:` (or enable compaction) and resume — the conversation overflowed the model's window.";
    case "WORKSPACE_SAFETY":
      return "Re-author the workflow so at most one branch holds a worktree-touching tool — multiple mutating branches collided.";
    case "TIMEOUT":
      return "Increase `max_duration` in the workflow's `budget:` block (or set a per-node timeout), then resume.";
    case "TOOL_FAILED_PERMANENT":
      return "Inspect the failing tool call in the Tools tab, fix the input or the tool itself, then resume.";
    case "SCHEMA_VALIDATION":
      return "Tighten the agent's prompt or relax the schema, then `iterion resume --force` (the workflow source has changed).";
    case "RESUME_INVALID":
      return "Add `--force` to the resume command to override the hash check — the workflow source changed since launch.";
    case "NETWORK_TRANSIENT":
      return "Resume to retry the LLM API call — a transient network blip interrupted the request.";
    default:
      // Suppress the hint for raw panics / stacktraces; otherwise point
      // the operator at the Events tab so they at least know where to
      // look next.
      if (run.error?.startsWith("panic:")) return null;
      // Sandbox start failures (docker postCreate, image pull races,
      // missing binaries inside the container) are recoverable from the
      // operator's side once the underlying infra issue is resolved.
      // Point at the dispatcher state docs so the operator can verify
      // docker is up, the image is reachable, and credentials mounted.
      if (run.error?.includes("sandbox: start") || run.error?.includes("postCreate")) {
        return "Sandbox start failed — verify `docker info` works, the image is reachable, and (for sandboxed claw) the `iterion` binary is on PATH inside the container. Resume retries the same sandbox bootstrap.";
      }
      return "Open the Events tab for the failing step's logs, then resume after addressing the root cause.";
  }
}

interface MergeStatusBadgeProps {
  status: RunHeaderType["merge_status"];
  strategy: RunHeaderType["merge_strategy"];
  merged: string;
  mergedShort: string;
  branch: string;
}

function MergeStatusBadge({
  status,
  strategy,
  merged,
  mergedShort,
  branch,
}: MergeStatusBadgeProps) {
  if (status === "merged" && merged) {
    return (
      <span className="ml-2 px-1.5 py-0.5 rounded bg-success-soft text-success-fg">
        {strategy === "squash" ? "squashed" : "merged"} into {merged}
        {mergedShort && (
          <span className="ml-1 font-mono text-success-fg/80">
            · {mergedShort}
          </span>
        )}
      </span>
    );
  }
  if (status === "pending") {
    return (
      <span className="ml-2 px-1.5 py-0.5 rounded bg-info-soft text-info-fg">
        awaiting merge — open Commits tab
      </span>
    );
  }
  if (status === "failed") {
    return (
      <span
        className="ml-2 px-1.5 py-0.5 rounded bg-danger-soft text-danger-fg"
        title="Open the left-panel Commits tab to retry."
      >
        merge failed — retry from Commits tab
      </span>
    );
  }
  if (status === "conflicted") {
    return (
      <span
        className="ml-2 px-1.5 py-0.5 rounded bg-warning-soft text-warning-fg"
        title="Open the Commits tab to resolve the conflict."
      >
        merge conflict — resolve in Commits tab
      </span>
    );
  }
  if (status === "skipped") {
    return (
      <span className="ml-2 text-fg-subtle">
        merge skipped — branch{" "}
        <code className="text-fg-default">{branch}</code> preserved
      </span>
    );
  }
  // Legacy runs (pre-merge_status) that recorded merged_into without a
  // status field. Keep the old wording.
  if (merged) {
    return (
      <span className="ml-2 px-1.5 py-0.5 rounded bg-success-soft text-success-fg">
        merged into {merged} ✓
      </span>
    );
  }
  if (branch) {
    return (
      <span className="ml-2 text-fg-subtle">
        not auto-merged — run{" "}
        <code className="text-fg-default">git merge {branch}</code>
      </span>
    );
  }
  return null;
}
