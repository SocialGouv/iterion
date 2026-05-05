import { useState } from "react";
import { useLocation } from "wouter";

import type { RunHeader as RunHeaderType } from "@/api/runs";
import { cancelRun, resumeRun } from "@/api/runs";
import { Button, StatusBadge } from "@/components/ui";

interface Props {
  run: RunHeaderType;
  active: boolean;
  wsState: string;
}

export default function RunHeader({ run, active, wsState }: Props) {
  const [, setLocation] = useLocation();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // canCancel includes "queued" so cloud-mode runs sitting on the NATS
  // queue can be aborted before a runner picks them up. The "active"
  // flag is only meaningful for in-process runs (local mode); a queued
  // run is never "active" in this server's process. See cloud-ready
  // plan §F (T-14).
  const canCancel =
    (run.status === "running" && active) || run.status === "queued";
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

  const onResume = async (force = false) => {
    setBusy(true);
    setError(null);
    try {
      await resumeRun(run.id, { force });
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const showFinalization = Boolean(run.final_commit);

  return (
    <header className="border-b border-border-default">
      <div className="px-4 py-2 flex items-center gap-3 text-sm">
        <button
          className="text-xs px-2 py-1 rounded bg-surface-2 hover:bg-surface-3"
          onClick={() => setLocation("/runs")}
        >
          ← Runs
        </button>
        <div className="flex flex-col leading-tight min-w-0 max-w-md">
          <div className="font-medium truncate">
            {run.name || run.workflow_name}
          </div>
          {run.name && (
            <div className="text-[10px] text-fg-subtle truncate">
              {run.workflow_name}
            </div>
          )}
        </div>
        <StatusBadge status={run.status} />
        {active && (
          <span
            className="inline-block w-1.5 h-1.5 rounded-full bg-info animate-pulse"
            title="Run is active in this server process"
          />
        )}
        {error && (
          <span className="text-[10px] text-danger truncate max-w-xs">{error}</span>
        )}
        <div className="ml-auto flex items-center gap-2">
          <span className="text-[10px] text-fg-subtle font-mono">{run.id}</span>
          <span
            className="text-[10px] text-fg-subtle"
            title={`WebSocket: ${wsState}`}
          >
            {wsState}
          </span>
          {canCancel && (
            <Button
              variant="danger"
              size="sm"
              onClick={() => void onCancel()}
              disabled={busy}
            >
              Cancel
            </Button>
          )}
          {canResume && (
            <>
              <Button
                variant="primary"
                size="sm"
                onClick={() => void onResume(false)}
                disabled={busy}
              >
                Resume
              </Button>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void onResume(true)}
                disabled={busy}
                title="Resume even if the workflow file has changed since launch (--force)"
              >
                Force
              </Button>
            </>
          )}
        </div>
      </div>
      {showFinalization && <FinalizationRow run={run} />}
    </header>
  );
}

// FinalizationRow surfaces the worktree-finalization outcome (commit
// SHA, storage branch, FF target) under the main header bar so the
// user can see at a glance whether the run's commits made it back to
// their branch — and what to do if they didn't. Only rendered when
// final_commit is set (i.e. the run produced commits in its worktree).
function FinalizationRow({ run }: { run: RunHeaderType }) {
  const [copied, setCopied] = useState<string | null>(null);
  const shortSha = (run.final_commit ?? "").slice(0, 7);
  const branch = run.final_branch ?? "";
  const merged = run.merged_into ?? "";

  const copy = async (text: string, key: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(key);
      setTimeout(() => setCopied(null), 1500);
    } catch {
      // clipboard may be unavailable (insecure context) — silent
    }
  };

  return (
    <div className="px-4 py-1.5 bg-surface-2/40 flex items-center gap-3 text-[11px] flex-wrap">
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
      {merged ? (
        <span className="ml-2 px-1.5 py-0.5 rounded bg-success-soft text-success-fg">
          merged into {merged} ✓
        </span>
      ) : branch ? (
        <span className="ml-2 text-fg-subtle">
          not auto-merged — run{" "}
          <code className="text-fg-default">git merge {branch}</code>
        </span>
      ) : null}
    </div>
  );
}
