import type { ManagerState } from "@/api/dispatcher";

const RUNNING_POLL_TITLE =
  "Poll the tracker now — due retries fire on the next tick.";
const PAUSED_POLL_TITLE =
  "Resume the dispatcher before polling for new dispatches or due retries.";
const START_FIRST_TITLE = "Start the dispatcher first";
const RELOAD_TITLE =
  "Reload dispatcher.yaml from disk without restarting the daemon.";

export interface DispatcherActionState {
  canPollDispatches: boolean;
  pollTitle: string;
  canReloadConfig: boolean;
  reloadTitle: string;
}

export function dispatcherActionState(
  state: ManagerState | null | undefined,
  snapshotPaused = false,
): DispatcherActionState {
  const isPaused = snapshotPaused || state === "paused";
  const canPollDispatches = state === "running" && !snapshotPaused;
  const canReloadConfig = state === "running" || state === "paused";

  return {
    canPollDispatches,
    pollTitle: isPaused
      ? PAUSED_POLL_TITLE
      : canPollDispatches
        ? RUNNING_POLL_TITLE
        : START_FIRST_TITLE,
    canReloadConfig,
    reloadTitle: canReloadConfig ? RELOAD_TITLE : START_FIRST_TITLE,
  };
}
