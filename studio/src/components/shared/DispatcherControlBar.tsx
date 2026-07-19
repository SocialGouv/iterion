import { errorMessage } from "@/lib/errorHints";
import { useCallback, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import * as dispatcher from "@/api/dispatcher";
import {
  dispatcherPillMeta,
  type DispatcherPillState,
} from "@/components/shared/dispatcherPillMeta";
import { Button } from "@/components/ui";
import { useConfirm } from "@/hooks/useConfirm";

interface Props {
  /** Called when the operator clicks the "Settings" button. */
  onOpenSettings: () => void;
  /** Optional override for the polling cadence; defaults to 2s. */
  pollIntervalMs?: number;
}

// DispatcherControlBar polls /api/v1/dispatcher/status and surfaces a
// state pill + the appropriate lifecycle buttons. Lives at the top of
// the Board + Dispatcher views so the operator can pilot the daemon
// from either one.
export default function DispatcherControlBar({ onOpenSettings, pollIntervalMs = 2000 }: Props) {
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const queryClient = useQueryClient();
  const statusQuery = useQuery<dispatcher.ManagerStatus>({
    queryKey: ["dispatcher-status"],
    queryFn: () => dispatcher.getStatus(),
    refetchInterval: pollIntervalMs,
    // The pill is the operator's dispatcher heartbeat: keep polling
    // while the tab is hidden so it is current the moment they return.
    refetchIntervalInBackground: true,
  });
  const status = statusQuery.data ?? null;
  // The poll error drives the "unreachable" pill; a lifecycle-action
  // failure fills in only when the poll has nothing to report.
  const error = statusQuery.error ? errorMessage(statusQuery.error) : actionError;

  const guard = useCallback(
    async (op: () => Promise<dispatcher.ManagerStatus>) => {
      setBusy(true);
      try {
        // Seed the cache with the returned status so the pill flips
        // immediately; the poll keeps it fresh from there.
        const s = await op();
        queryClient.setQueryData<dispatcher.ManagerStatus>(["dispatcher-status"], s);
        setActionError(null);
      } catch (e) {
        setActionError(errorMessage(e));
      } finally {
        setBusy(false);
      }
    },
    [queryClient],
  );

  const { confirm, dialog } = useConfirm();
  const stopWithConfirm = useCallback(async () => {
    const ok = await confirm({
      title: "Stop the dispatcher?",
      message:
        "This disposes the dispatcher and clears its eligible queue. You'll need to Start it again to resume polling.",
      confirmLabel: "Stop",
      confirmVariant: "danger",
    });
    if (ok) void guard(dispatcher.stop);
  }, [confirm, guard]);

  const pill = renderPill(status, error);
  const state = status?.state ?? "idle";
  const hasConfig = status?.has_config ?? false;

  return (
    <div className="flex items-center gap-2 border-b border-border-default bg-surface-1 px-4 py-2 text-xs">
      {pill}
      {status?.last_error && (
        <span className="ml-2 max-w-[400px] truncate text-danger-fg" title={status.last_error}>
          ⚠ {status.last_error}
        </span>
      )}
      <div className="ml-auto flex items-center gap-2">
        {(state === "idle" || state === "error") && (
          <Button
            variant="primary"
            size="sm"
            disabled={busy || !hasConfig}
            aria-label="Start the dispatcher"
            title={hasConfig ? "Start the dispatcher" : "Save a config in Settings first"}
            onClick={() => void guard(dispatcher.start)}
          >
            ▶ Start
          </Button>
        )}
        {state === "running" && (
          <>
            <Button
              variant="secondary"
              size="sm"
              disabled={busy}
              aria-label="Pause the dispatcher"
              onClick={() => void guard(dispatcher.pause)}
            >
              ⏸ Pause
            </Button>
            <Button
              variant="danger"
              size="sm"
              disabled={busy}
              aria-label="Stop the dispatcher"
              onClick={() => void stopWithConfirm()}
            >
              ■ Stop
            </Button>
          </>
        )}
        {state === "paused" && (
          <>
            <Button
              variant="primary"
              size="sm"
              disabled={busy}
              aria-label="Resume the dispatcher"
              onClick={() => void guard(dispatcher.resume)}
            >
              ▶ Resume
            </Button>
            <Button
              variant="danger"
              size="sm"
              disabled={busy}
              aria-label="Stop the dispatcher"
              onClick={() => void stopWithConfirm()}
            >
              ■ Stop
            </Button>
          </>
        )}
        <Button
          variant="ghost"
          size="sm"
          aria-label="Dispatcher settings"
          onClick={onOpenSettings}
        >
          ⚙ Settings
        </Button>
      </div>
      {dialog}
    </div>
  );
}

function renderPill(status: dispatcher.ManagerStatus | null, fetchError: string | null) {
  const pillState: DispatcherPillState =
    fetchError && !status ? "unreachable" : status?.state ?? "idle";
  const meta = dispatcherPillMeta(pillState);
  // Append the uptime anchor whenever the daemon reports a start time —
  // the server keeps started_at set across pause/error, not just while
  // running, so this matches the pre-refactor pill which showed
  // "since …" in every state that had one.
  const title = status?.started_at
    ? `${meta.title} · since ${status.started_at}`
    : meta.title;
  return (
    <span
      className={`rounded px-2 py-0.5 font-medium ${meta.className}`}
      title={title}
    >
      {meta.label}
    </span>
  );
}
