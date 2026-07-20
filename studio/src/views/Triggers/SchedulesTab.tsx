import { useCallback, useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useLocation } from "wouter";

import { listBots, type BotEntryWithSchema } from "@/api/bots";
import { FeatureUnavailableError } from "@/api/client";
import { forgeTeamRepoKey } from "@/api/forgeConnections";
import {
  deleteTeamSchedule,
  listTeamSchedules,
  updateTeamSchedule,
  type ScheduledBot,
} from "@/api/schedules";
import { formatDateTime } from "@/lib/format";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useCanManageTeam } from "@/hooks/useCanManageTeam";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
import { humanizeCron } from "@/lib/humanizeCron";

import { EditScheduleDialog, NewScheduleDialog } from "./ScheduleDialogs";
import {
  cadenceLabel,
  filterGroupsByRepo,
  formatNextFire,
  groupSchedulesByRepo,
  isAlwaysOn,
  type ScheduleRepoGroup,
} from "./scheduleModel";

// Stable empty fallback so the undefined→loaded transition doesn't hand
// downstream memos a fresh [] reference each render.
const EMPTY_BOTS: BotEntryWithSchema[] = [];

// SchedulesTab is the management surface for a team's scheduled bots
// (cloudsched rows) inside Automations: grouped by repository via the
// client-side join against the connected-repos list, with pause/resume,
// policy editing, creation and deletion in place.
export default function SchedulesTab({
  repoFilterParam,
  creating,
  onCreatingChange,
  onUnavailable,
}: {
  /** Deep-link pre-filter (?repo=owner/repo) — null shows all repos. */
  repoFilterParam: string | null;
  /** "New schedule" dialog visibility — owned by the view header. */
  creating: boolean;
  onCreatingChange: (open: boolean) => void;
  /** Reports "no schedule store on this server" up so the view header
   *  can hide its New-schedule button instead of offering a no-op. */
  onUnavailable: () => void;
}) {
  const { repos, enabled, teamID } = useActiveRepo();
  const canManage = useCanManageTeam();
  const [, navigate] = useLocation();
  const { confirm, dialog } = useConfirm();

  const [actionErr, setActionErr] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [editing, setEditing] = useState<ScheduledBot | null>(null);

  // Deep link wins on arrival; the operator can widen back to all repos.
  const [repoFilter, setRepoFilter] = useState<string | null>(repoFilterParam);
  useEffect(() => setRepoFilter(repoFilterParam), [repoFilterParam]);

  const queryClient = useQueryClient();
  const schedulesQuery = useQuery<ScheduledBot[]>({
    queryKey: ["team-schedules", teamID],
    queryFn: () => listTeamSchedules(teamID ?? ""),
    enabled: !!teamID,
  });
  const schedules = schedulesQuery.data ?? null;
  // No schedule store on this server: the tab swaps to its EmptyState,
  // and the view header hides its New-schedule button (reported up).
  const unavailable = schedulesQuery.error instanceof FeatureUnavailableError;
  useEffect(() => {
    if (unavailable) onUnavailable();
  }, [unavailable, onUnavailable]);
  // Mutation failures overlay the fetch error; reload() clears them.
  const err =
    actionErr ??
    (schedulesQuery.error && !unavailable
      ? errorMessage(schedulesQuery.error)
      : null);

  const reload = useCallback(() => {
    setActionErr(null);
    return queryClient.invalidateQueries({ queryKey: ["team-schedules", teamID] });
  }, [queryClient, teamID]);

  // Bot personas are decoration — a registry failure must not block the
  // list (the query holds the error; nothing renders it).
  const botsQuery = useQuery<BotEntryWithSchema[]>({
    queryKey: ["bots"],
    queryFn: () => listBots(),
  });
  const bots = botsQuery.data ?? EMPTY_BOTS;

  const groups = useMemo(
    () => groupSchedulesByRepo(schedules ?? [], repos),
    [schedules, repos],
  );
  const visible = useMemo(
    () => filterGroupsByRepo(groups, repoFilter),
    [groups, repoFilter],
  );

  const botLabel = useCallback(
    (id: string) => bots.find((b) => b.name === id)?.display_name || id,
    [bots],
  );

  // Pre-select the filtered repo in the create dialog.
  const filteredRepo = useMemo(
    () => (repoFilter ? repos.find((r) => r.repo_full_name === repoFilter) ?? null : null),
    [repoFilter, repos],
  );

  if (!enabled || unavailable) {
    return (
      <EmptyState
        title="Schedules not available here"
        message={
          enabled
            ? "This server has no schedule store wired. Recurring local bots run via `iterion schedule` on the host crontab instead."
            : "Scheduled bots are a cloud feature — sign in to a cloud studio with a team to manage recurring runs."
        }
      />
    );
  }

  const toggle = async (s: ScheduledBot) => {
    if (!teamID) return;
    setBusy(s.id);
    try {
      await updateTeamSchedule(teamID, s.id, { disabled: !s.disabled });
      await reload();
    } catch (e) {
      setActionErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  const remove = async (s: ScheduledBot) => {
    if (!teamID) return;
    const ok = await confirm({
      title: "Delete schedule?",
      message: `Stop running ${botLabel(s.bot_id)} on this cadence? The bot and its repo wiring stay; only the schedule goes.`,
      confirmLabel: "Delete schedule",
      confirmVariant: "danger",
    });
    if (!ok) return;
    setBusy(s.id);
    try {
      await deleteTeamSchedule(teamID, s.id);
      await reload();
    } catch (e) {
      setActionErr(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  const total = schedules?.length ?? 0;

  return (
    <div className="flex flex-col gap-3">
      {dialog}
      <p className="text-xs text-fg-muted">
        Recurring bot runs on a cron cadence (UTC), grouped by the repository they
        target. Webhook-provisioned schedules appear here too.
      </p>

      {err && (
        <InlineBanner tone="danger" title="Couldn't load schedules">
          {err}
        </InlineBanner>
      )}

      {repoFilter && (
        <div className="flex items-center gap-2 text-xs text-fg-muted">
          <span>
            Filtered to{" "}
            <span className="font-mono text-fg-default">{repoFilter}</span>
          </span>
          <Button variant="ghost" size="sm" onClick={() => setRepoFilter(null)}>
            Show all repos
          </Button>
        </div>
      )}

      {schedules === null && !err ? (
        <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
          <Spinner /> Loading schedules…
        </div>
      ) : total === 0 ? (
        repos.length === 0 ? (
          <EmptyState
            title="No schedules yet"
            message="Schedules usually target a connected repository. Connect one to enable bots on it, then put them on a cadence."
            action={
              <Button
                variant="primary"
                size="sm"
                onClick={() => navigate("/integrations/connect")}
              >
                Connect a repository
              </Button>
            }
          />
        ) : (
          <EmptyState
            title="No schedules yet"
            message="Put a bot on a cadence — e.g. a nightly docs refresh or a weekly security audit."
            action={
              canManage ? (
                <Button variant="primary" size="sm" onClick={() => onCreatingChange(true)}>
                  New schedule
                </Button>
              ) : undefined
            }
          />
        )
      ) : visible.length === 0 ? (
        <EmptyState
          title="No schedules for this repository"
          message="The current filter matches nothing — other repositories have schedules."
          action={
            <Button variant="secondary" size="sm" onClick={() => setRepoFilter(null)}>
              Show all repos
            </Button>
          }
        />
      ) : (
        visible.map((g) => (
          <ScheduleGroupCard
            key={g.key ?? "__unlinked"}
            group={g}
            botLabel={botLabel}
            canManage={canManage}
            busy={busy}
            onToggle={toggle}
            onEdit={setEditing}
            onRemove={remove}
          />
        ))
      )}

      {teamID && (
        <>
          <NewScheduleDialog
            open={creating}
            onOpenChange={onCreatingChange}
            onCreated={() => {
              onCreatingChange(false);
              void reload();
            }}
            teamID={teamID}
            repos={repos}
            bots={bots}
            defaultRepoKey={filteredRepo ? forgeTeamRepoKey(filteredRepo) : null}
          />
          <EditScheduleDialog
            teamID={teamID}
            schedule={editing}
            onOpenChange={(open) => {
              if (!open) setEditing(null);
            }}
            onSaved={() => {
              setEditing(null);
              void reload();
            }}
          />
        </>
      )}
    </div>
  );
}

function ScheduleGroupCard({
  group,
  botLabel,
  canManage,
  busy,
  onToggle,
  onEdit,
  onRemove,
}: {
  group: ScheduleRepoGroup;
  botLabel: (id: string) => string;
  canManage: boolean;
  busy: string | null;
  onToggle: (s: ScheduledBot) => void | Promise<void>;
  onEdit: (s: ScheduledBot) => void;
  onRemove: (s: ScheduledBot) => void | Promise<void>;
}) {
  return (
    <section>
      <h3 className="mb-1 flex items-center gap-2 text-xs font-medium text-fg-default">
        {group.key !== null ? (
          <span className="font-mono">{group.label}</span>
        ) : (
          <span>{group.label}</span>
        )}
        {group.repo && (
          <span className="rounded bg-surface-2 px-1.5 py-0.5 text-caption font-normal text-fg-muted">
            {group.repo.provider}
          </span>
        )}
      </h3>
      <ul className="divide-y divide-border-subtle rounded border border-border-subtle bg-surface-1">
        {group.schedules.map((s) => (
          <li key={s.id} className="flex flex-wrap items-center gap-2 px-3 py-2 text-xs">
            <span className="font-medium text-fg-default">{botLabel(s.bot_id)}</span>
            {botLabel(s.bot_id) !== s.bot_id && (
              <span className="font-mono text-fg-subtle">{s.bot_id}</span>
            )}
            {isAlwaysOn(s) ? (
              <>
                <span
                  className="text-fg-muted"
                  title={
                    s.interval_seconds
                      ? `relaunches every ${s.interval_seconds}s`
                      : "always-on"
                  }
                >
                  {cadenceLabel(s)}
                </span>
                <Badge
                  variant="accent"
                  size="sm"
                  title={`Always-on: at most one live run; a run silent past ${s.stale_after || "5m"} is relaunched`}
                >
                  always-on
                </Badge>
              </>
            ) : (
              <span className="text-fg-muted" title={`cron: ${s.cron} (UTC)`}>
                {humanizeCron(s.cron) ?? s.cron}
              </span>
            )}
            {s.overlap === "allow" && (
              <Badge variant="neutral" size="sm" title="Overlapping runs allowed">
                overlap: allow{s.max_concurrent ? ` ≤${s.max_concurrent}` : ""}
              </Badge>
            )}
            {s.guard && (
              <Badge variant="neutral" size="sm" title={`Guard: ${s.guard}`}>
                guarded
              </Badge>
            )}
            {s.disabled ? (
              <Badge variant="neutral" size="sm">
                paused
              </Badge>
            ) : (
              <span
                className="text-fg-subtle"
                title={formatDateTime(s.next_fire_at)}
              >
                next run {formatNextFire(s.next_fire_at)}
              </span>
            )}
            <span className="ml-auto" />
            {canManage && (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy === s.id}
                  onClick={() => void onToggle(s)}
                >
                  {s.disabled ? "Resume" : "Pause"}
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={busy === s.id}
                  onClick={() => onEdit(s)}
                >
                  Edit
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  className="text-danger"
                  disabled={busy === s.id}
                  onClick={() => void onRemove(s)}
                >
                  Delete
                </Button>
              </>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}
