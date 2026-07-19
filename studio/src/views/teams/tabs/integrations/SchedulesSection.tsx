import { useQuery } from "@tanstack/react-query";
import { useLocation } from "wouter";

import { listTeamSchedules } from "@/api/schedules";
import { FeatureUnavailableError } from "@/api/client";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { errorMessage } from "@/lib/errorHints";
import { humanizeCron } from "@/lib/humanizeCron";
import { formatNextFire, nextUpcoming } from "@/views/Triggers/scheduleModel";

// SchedulesSection is a compact pointer: schedules are managed in
// Automations → Schedules (grouped by repo, pause/edit/create/delete);
// this section only surfaces that they exist — count + the next firing
// one — so the repo-enable flow's output stays discoverable from here.
export default function SchedulesSection({
  teamID,
}: {
  teamID: string;
  canManage: boolean;
}) {
  const [, navigate] = useLocation();

  const query = useQuery({
    queryKey: ["team-schedules", teamID],
    queryFn: () => listTeamSchedules(teamID),
  });
  const schedules = query.data;
  // Local mode has no schedule store (404): the section only exists
  // where the feature does. Any other failure must stay visible.
  const err =
    query.error && !(query.error instanceof FeatureUnavailableError)
      ? errorMessage(query.error)
      : null;

  if (err) {
    return (
      <InlineBanner tone="danger" layout="inline" title="Couldn't load schedules">
        {err}
      </InlineBanner>
    );
  }
  if (!schedules || schedules.length === 0) return null;

  const next = nextUpcoming(schedules);
  const paused = schedules.filter((s) => s.disabled).length;

  return (
    <div>
      <h3 className="font-medium mb-1">Scheduled bots</h3>
      <div className="flex flex-wrap items-center gap-2 rounded border border-border-subtle bg-surface-1 px-3 py-2 text-xs">
        <span className="text-fg-default">
          {schedules.length} schedule{schedules.length > 1 ? "s" : ""}
          {paused > 0 && <span className="text-fg-muted"> ({paused} paused)</span>}
        </span>
        {next && (
          <span className="text-fg-muted" title={`cron: ${next.cron} (UTC)`}>
            next: {next.bot_id} {formatNextFire(next.next_fire_at)}
            {humanizeCron(next.cron) ? ` (${humanizeCron(next.cron)})` : ""}
          </span>
        )}
        <span className="ml-auto" />
        <Button
          variant="ghost"
          size="sm"
          onClick={() => navigate("/triggers?tab=schedules")}
        >
          Manage in Automations
        </Button>
      </div>
    </div>
  );
}
