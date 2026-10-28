import { useRunStore } from "@/store/run";

// The caller's effective standing for the open run (ADR-143), resolved
// server-side at the run-by-id choke point and shipped on every
// snapshot. A viewer — a team viewer, an org-admin-lineage reader, an
// org-shared reader — reads but never acts: every affordance that would
// POST/PUT or steer through the socket gates on this. Undefined on
// auth-less local servers: today's unconditional affordances.
export function useRunCanAct(): boolean {
  const viewer = useRunStore((s) => s.viewer);
  return viewer ? viewer.role !== "viewer" : true;
}
