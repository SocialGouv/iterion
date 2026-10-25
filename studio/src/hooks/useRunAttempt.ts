import { useMemo } from "react";
import { useRunStore } from "@/store/run";
import { attemptExecutions, scopeAttempt } from "@/lib/runAttempt";
import type { RunAttempt } from "@/api/runs";

const emptyAttempts: RunAttempt[] = [];

export function useRunAttempt() {
  const events = useRunStore((s) => s.events);
  const attempts = useRunStore((s) => s.snapshot?.run.attempts ?? emptyAttempts);
  const selectedID = useRunStore((s) => s.selectedAttemptID);
  const live = useRunStore((s) => s.executionsById);
  const scope = useMemo(() => scopeAttempt(events, attempts, selectedID), [events, attempts, selectedID]);
  const executionsById = useMemo(() => attemptExecutions(scope, live), [scope, live]);
  return { ...scope, attempts, executionsById };
}
