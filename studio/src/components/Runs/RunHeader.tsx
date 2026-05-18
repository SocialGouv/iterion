import { useEffect, useRef, useState } from "react";
import { useLocation } from "wouter";
import { Pencil1Icon } from "@radix-ui/react-icons";

import type { RunHeader as RunHeaderType } from "@/api/runs";
import { cancelRun, getRun, loadEvents } from "@/api/runs";
import { Button, CopyButton, IconButton, LiveDot, StatusBadge, Tooltip } from "@/components/ui";
import AppHeader from "@/components/shared/AppHeader";
import WSStatusDot from "@/components/shared/WSStatusDot";
import { useRunStore } from "@/store/run";

import ResumeDialog from "./ResumeDialog";

interface Props {
  run: RunHeaderType;
  active: boolean;
  wsState: string;
}

export default function RunHeader({ run, active, wsState }: Props) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [resumeOpen, setResumeOpen] = useState(false);
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
    run.status === "queued";
  // Resume from header is a "best-effort" trigger — for paused_waiting_human
  // runs the user normally fills the Pause form in the detail panel
  // (Phase 5). The header button stays for failed_resumable / cancelled
  // runs which don't need answers.
  const canResume =
    run.status === "failed_resumable" || run.status === "cancelled";

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

  return (
    <>
      <AppHeader active="runs" />
      <div className="shrink-0 border-b border-border-default px-4 py-2 flex items-center gap-3 text-sm">
        <div className="flex flex-col leading-tight min-w-0 max-w-md">
          <Tooltip content={run.name || run.workflow_name}>
            <div className="font-medium truncate" tabIndex={0}>
              {run.name || run.workflow_name}
            </div>
          </Tooltip>
          {run.name && (
            <Tooltip content={run.workflow_name}>
              <div className="text-[10px] text-fg-subtle truncate" tabIndex={0}>
                {run.workflow_name}
              </div>
            </Tooltip>
          )}
        </div>
        {run.file_path && (
          <IconButton
            label="Open workflow in editor"
            tooltip={`Open ${run.file_path} in the studio`}
            size="sm"
            variant="ghost"
            onClick={() =>
              setLocation(
                `/editor?file=${encodeURIComponent(run.file_path!)}&from=${encodeURIComponent(run.id)}`,
              )
            }
          >
            <Pencil1Icon />
          </IconButton>
        )}
        <StatusBadge status={run.status} />
        {active && (
          <LiveDot
            tone="info"
            size="sm"
            label="Run is active in this server process"
          />
        )}
        {error && (
          <span className="text-[10px] text-danger truncate max-w-xs">{error}</span>
        )}
        <div className="ml-auto flex items-center gap-2">
          <span
            className="text-[10px] text-fg-subtle font-mono"
            title="Run ID"
          >
            {run.id}
          </span>
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
          {/*
            Only render the Cancel button when the run is actually
            cancellable. A disabled button on a terminal/untracked run
            is visual noise — the StatusBadge already says "finished"
            / "failed" / "cancelled" so the user knows there's nothing
            to act on. Hiding it cleans up the header.
          */}
          {canCancel && (
            <Button
              variant="danger"
              size="sm"
              onClick={() => void onCancel()}
              disabled={busy}
              title="Cancel this run"
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
              title="Resume this run from its last checkpoint"
            >
              Resume…
            </Button>
          )}
        </div>
      </div>
      <WSDisconnectBanner state={wsState} />
      {showFinalization && <FinalizationRow run={run} />}
      <ErrorHintRow run={run} onResume={() => setResumeOpen(true)} />

      {canResume && (
        <ResumeDialog
          run={run}
          open={resumeOpen}
          onOpenChange={setResumeOpen}
        />
      )}
    </>
  );
}

// WSDisconnectBanner shows an inline warning row when the run-console
// WebSocket has been closed for at least 2 seconds. Without this banner
// the only signal that live updates have stopped is the small WSStatusDot
// in the header — easy to miss while watching the event log. The 2-second
// debounce avoids flicker during normal reconnect blips.
function WSDisconnectBanner({ state }: { state: string }) {
  const [showStale, setShowStale] = useState(false);
  const requestReconnect = useRunStore((s) => s.requestWsReconnect);
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
      className="px-4 py-1.5 bg-warning-soft border-b border-warning/40 flex items-center gap-2 text-[11px] text-warning-fg"
    >
      <LiveDot tone="danger" size="sm" pulse={false} />
      <span>Live updates disconnected — data may be stale.</span>
      <Button
        variant="ghost"
        size="sm"
        className="ml-auto"
        onClick={() => requestReconnect()}
      >
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
      return `Budget exhausted. Raise the workflow's \`budget:\` block (max_cost_usd, max_tokens, max_iterations, or max_duration) then \`iterion resume --run-id ${run.id}${
        run.file_path ? ` --file ${run.file_path}` : ""
      } --force\` to continue past the original budget.`;
    case "RATE_LIMITED":
      return "Upstream provider rate-limited the request. Wait a few minutes then resume — the engine will retry from the failed node.";
    case "LOOP_EXHAUSTED":
      return "A loop hit its iteration cap. Either raise the loop's `(N)` count in the workflow, or accept the partial output and let the run finish.";
    case "CONTEXT_LENGTH_EXCEEDED":
      return "Conversation context overflowed the model's window. Lower the per-node compaction `ratio:` (or enable compaction) and resume.";
    case "WORKSPACE_SAFETY":
      return "Multiple mutating branches tried to touch the workspace concurrently. Re-author the workflow so at most one branch holds a worktree-touching tool.";
    case "TIMEOUT":
      return "A node exceeded its time budget. Increase `max_duration` in the workflow's `budget:` block or set a per-node timeout, then resume.";
    case "TOOL_FAILED_PERMANENT":
      return "A tool returned a non-retryable error. Inspect the failing tool call in the Tools tab, fix the input or the tool itself, then resume.";
    case "SCHEMA_VALIDATION":
      return "An agent's structured output didn't match its schema. Tighten the prompt or relax the schema, then `iterion resume --force` (workflow source changed).";
    case "RESUME_INVALID":
      return "Resume rejected: the workflow source changed since the run started. Add `--force` to the resume command to override the hash check.";
    case "NETWORK_TRANSIENT":
      return "Network blip while reaching the LLM API. Resume — the engine will retry with backoff.";
    default:
      return null;
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
      <span className="ml-2 px-1.5 py-0.5 rounded bg-danger-soft text-danger-fg">
        merge failed — retry from Commits tab
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
