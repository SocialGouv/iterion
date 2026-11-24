import LastRunOutcome from "@/components/shared/LastRunOutcome";
import type { ScheduledBot } from "@/api/schedules";

// The schedule's instance of the shared run-outcome note (#1501): the tuple
// renders once, in one place, and this surface owns only what is true of a
// SCHEDULE — a failed tick does not stop the cadence, so the operator is told
// what will happen next.
export default function ScheduleOutcome({ schedule: s }: { schedule: ScheduledBot }) {
  return (
    <LastRunOutcome
      record={s}
      trailer={s.disabled ? "Schedule paused." : "The schedule keeps its cadence. Pause it while fixing the cause if needed."}
    />
  );
}
