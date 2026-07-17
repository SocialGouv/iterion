import { useCallback, useEffect, useState } from "react";

import { errorMessage } from "@/lib/errorHints";
import {
  deleteTeamSchedule,
  listTeamSchedules,
  updateTeamSchedule,
  type ScheduledBot,
} from "@/api/schedules";
import { FeatureUnavailableError } from "@/api/client";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useConfirm } from "@/hooks/useConfirm";
import { humanizeCron } from "@/lib/humanizeCron";
import { formatRelative } from "@/lib/format";

// SchedulesSection lists the team's scheduled bots — the recurring runs
// EnableRepoPanel provisions alongside a repo's webhook. Until now they
// vanished after creation (REST-only); this is their management surface:
// see the cadence, pause/resume, or remove one. Creation stays with the
// repo-enable flow so a schedule is always born bound to a repo.
export default function SchedulesSection({
  teamID,
  canManage,
}: {
  teamID: string;
  canManage: boolean;
}) {
  const [schedules, setSchedules] = useState<ScheduledBot[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const { confirm, dialog } = useConfirm();

  const reload = useCallback(async () => {
    try {
      setErr(null);
      setSchedules(await listTeamSchedules(teamID));
    } catch (e) {
      if (e instanceof FeatureUnavailableError) {
        setUnavailable(true);
        return;
      }
      setErr(errorMessage(e));
    }
  }, [teamID]);

  useEffect(() => {
    void reload();
  }, [reload]);

  // Server without the schedules store (local mode): render nothing —
  // the section only exists where the feature does.
  if (unavailable) return null;
  if (schedules !== null && schedules.length === 0 && !err) return null;

  const repoLabel = (s: ScheduledBot): string => {
    if (!s.repo_url) return "";
    return s.repo_url
      .replace(/^https?:\/\/[^/]+\//, "")
      .replace(/\.git$/, "");
  };

  const toggle = async (s: ScheduledBot) => {
    setBusy(s.id);
    try {
      await updateTeamSchedule(teamID, s.id, { disabled: !s.disabled });
      await reload();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  const remove = async (s: ScheduledBot) => {
    const ok = await confirm({
      title: "Remove schedule?",
      message: `Stop running ${s.bot_id} on this cadence? The bot and its repo wiring stay; only the schedule goes.`,
      confirmLabel: "Remove schedule",
      confirmVariant: "danger",
    });
    if (!ok) return;
    setBusy(s.id);
    try {
      await deleteTeamSchedule(teamID, s.id);
      await reload();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div>
      {dialog}
      <h3 className="font-medium mb-1">Scheduled bots</h3>
      <p className="text-xs text-fg-muted mb-3">
        Recurring runs on a cron (UTC). New schedules are created when
        enabling a bot on a repository.
      </p>
      {err && (
        <InlineBanner tone="danger" layout="inline" className="mb-2">
          {err}
        </InlineBanner>
      )}
      <ul className="divide-y divide-border-subtle rounded border border-border-subtle bg-surface-1">
        {(schedules ?? []).map((s) => (
          <li key={s.id} className="flex flex-wrap items-center gap-2 px-3 py-2 text-xs">
            <span className="font-medium text-fg-default">{s.bot_id}</span>
            {repoLabel(s) && (
              <span className="font-mono text-fg-subtle truncate max-w-[16rem]">
                {repoLabel(s)}
              </span>
            )}
            <span
              className="text-fg-muted"
              title={`cron: ${s.cron} (UTC)`}
            >
              {humanizeCron(s.cron) ?? s.cron}
            </span>
            {s.disabled ? (
              <Badge variant="neutral" size="sm">
                paused
              </Badge>
            ) : (
              <span
                className="text-fg-subtle"
                title={new Date(s.next_fire_at).toLocaleString()}
              >
                next {formatRelative(s.next_fire_at)}
              </span>
            )}
            <span className="ml-auto" />
            {canManage && (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy === s.id}
                  onClick={() => void toggle(s)}
                >
                  {s.disabled ? "Resume" : "Pause"}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  className="text-danger"
                  disabled={busy === s.id}
                  onClick={() => void remove(s)}
                >
                  Remove
                </Button>
              </>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}
