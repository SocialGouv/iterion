// dispatcherPillMeta centralises the copy + colour for the dispatcher
// state pill rendered by DispatcherControlBar. The pill is the
// operator's primary "is the daemon alive?" signal, so every state
// carries a hover explanation — previously `idle` and `unreachable`
// rendered with no title at all.
//
// Extracted as a pure helper (mirroring runStatusMeta.ts) so the
// per-state wording can be unit-tested without mounting the control
// bar.

// DispatcherPillState is the set of states the pill can show. The first
// four mirror dispatcher.ManagerState; "unreachable" is a synthetic UI
// state used when the status endpoint can't be reached at all (the
// `iterion dispatch` process is down or was never started).
export type DispatcherPillState =
  | "idle"
  | "running"
  | "paused"
  | "error"
  | "unreachable";

export interface DispatcherPillMeta {
  // Full pill text, e.g. "dispatcher: running" or "unreachable".
  label: string;
  // Hover explanation shown via the pill's `title`.
  title: string;
  // Tailwind colour classes (background + foreground). Layout classes
  // (rounded / padding / weight) live in the component so the pill
  // geometry stays uniform across states.
  className: string;
}

export function dispatcherPillMeta(
  state: DispatcherPillState,
): DispatcherPillMeta {
  switch (state) {
    case "running":
      return {
        label: "dispatcher: running",
        title: "Polling the tracker and dispatching ready issues.",
        className: "bg-green-500/20 text-green-300",
      };
    case "paused":
      return {
        label: "dispatcher: paused",
        title:
          "Lifecycle paused — in-flight runs continue; no new dispatches until you Resume.",
        className: "bg-amber-500/20 text-amber-300",
      };
    case "error":
      return {
        label: "dispatcher: error",
        title:
          "The dispatcher hit an error — see the message alongside the pill.",
        className: "bg-red-500/20 text-red-300",
      };
    case "unreachable":
      return {
        label: "unreachable",
        title:
          "Can't reach the dispatcher API — the `iterion dispatch` process may have exited.",
        className: "bg-red-500/20 text-red-300",
      };
    case "idle":
      return {
        label: "dispatcher: idle",
        title:
          "No dispatcher attached. Click Start (needs a saved config) to begin polling the tracker.",
        className: "bg-fg-muted/20 text-fg-muted",
      };
    default:
      // Defensive: a runtime state outside the known enum (e.g. a new
      // server ManagerState not yet mirrored here) keeps its real name
      // with a neutral pill, rather than being silently relabeled
      // "idle" — which would also mis-offer the Start button.
      return {
        label: `dispatcher: ${String(state)}`,
        title:
          "Unrecognised dispatcher state — the studio may be out of date.",
        className: "bg-fg-muted/20 text-fg-muted",
      };
  }
}
