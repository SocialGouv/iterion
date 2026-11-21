import { useRunStore, type RunStoreState } from "@/store/run";

// The caller's effective standing for the open run (ADR-148), resolved
// server-side at the run-by-id choke point and shipped on every
// snapshot. A viewer — a team viewer, an org-admin-lineage reader, an
// org-shared reader — reads but never acts: every affordance that would
// POST/PUT or steer through the socket gates on this. Undefined on
// auth-less local servers: today's unconditional affordances.
//
// A view whose socket closed scope_lost (wsState "lost") acts on nothing
// either: the run it shows was replaced or deleted under its ID, or the
// caller's access changed, so a command sent by that ID would land on
// whatever run holds it now.
export function runCanAct(
  viewer: { role: string } | null | undefined,
  wsState: RunStoreState["wsState"],
): boolean {
  if (wsState === "lost") return false;
  return viewer ? viewer.role !== "viewer" : true;
}

export function useRunCanAct(): boolean {
  const viewer = useRunStore((s) => s.viewer);
  const wsState = useRunStore((s) => s.wsState);
  return runCanAct(viewer, wsState);
}
