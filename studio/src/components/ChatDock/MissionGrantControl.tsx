// "See this run through" — the control that turns a per-step confirmation
// into one bounded delegation.
//
// It sits beside Confirm rather than replacing it: the operator who wants to
// approve each step keeps doing exactly that. What this adds is the case the
// per-action policy could not express — delegating ONE job on ONE run for a
// bounded time, instead of granting a verb everywhere and forever.

import { useCallback, useEffect, useState } from "react";

import { Button } from "@/components/ui/Button";
import type { AssistantActionId } from "@/lib/chatDock/assistantActions";
import {
  GRANTABLE_ACTIONS,
  MISSION_GRANT_EVENT,
  grantMission,
  missionGrantCovers,
  revokeMission,
} from "@/lib/chatDock/missionGrant";

// Two hours: long enough to cover a repair that waits on several agent turns,
// short enough that a grant given tonight is not still live tomorrow. There is
// deliberately no "until I revoke" option — the axis operators forget is time,
// and an expiry they never chose is the one that protects them.
const MISSION_TTL_MS = 2 * 60 * 60 * 1000;

export interface MissionGrantControlProps {
  assistantRunId: string | null;
  targetRunId: string | null;
  action: AssistantActionId;
  assistantLabel: string;
}

export default function MissionGrantControl({
  assistantRunId,
  targetRunId,
  action,
  assistantLabel,
}: MissionGrantControlProps) {
  const [covered, setCovered] = useState(() =>
    missionGrantCovers(assistantRunId, targetRunId, action),
  );

  useEffect(() => {
    const refresh = () =>
      setCovered(missionGrantCovers(assistantRunId, targetRunId, action));
    refresh();
    window.addEventListener(MISSION_GRANT_EVENT, refresh);
    window.addEventListener("storage", refresh);
    return () => {
      window.removeEventListener(MISSION_GRANT_EVENT, refresh);
      window.removeEventListener("storage", refresh);
    };
  }, [assistantRunId, targetRunId, action]);

  const grant = useCallback(() => {
    if (!assistantRunId || !targetRunId) return;
    grantMission({
      assistantRunId,
      targetRunId,
      // The whole grantable set, not just the verb in front of us: a repair
      // that can rewind but must stop to ask before resuming has not been
      // delegated, it has been half-delegated, and it stalls at step two.
      actions: [...GRANTABLE_ACTIONS],
      ttlMs: MISSION_TTL_MS,
    });
  }, [assistantRunId, targetRunId]);

  const revoke = useCallback(() => {
    if (!assistantRunId || !targetRunId) return;
    revokeMission(assistantRunId, targetRunId);
  }, [assistantRunId, targetRunId]);

  // A grant is meaningless without both ends: no assistant to bind it to, or
  // no run to bound it by, and the control would promise a scope it cannot
  // enforce.
  if (!assistantRunId || !targetRunId) return null;
  if (!GRANTABLE_ACTIONS.includes(action)) return null;

  if (covered) {
    return (
      <button
        type="button"
        onClick={revoke}
        className="text-caption text-muted-fg underline underline-offset-2 hover:text-fg-default"
      >
        {assistantLabel} is seeing this run through — stop delegating
      </button>
    );
  }
  return (
    <Button variant="secondary" size="sm" onClick={grant}>
      Let {assistantLabel} see this run through
    </Button>
  );
}
