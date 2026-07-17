import { useCallback, useEffect, useState } from "react";

import { errorMessage } from "@/lib/errorHints";
import {
  deleteTeamSchedule,
  listTeamSchedules,
  updateTeamSchedule,
  type ScheduledBot,
  type SchedulePatch,
} from "@/api/schedules";
import { FeatureUnavailableError } from "@/api/client";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
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
  // Schedule id whose tick-policy editor is open ("" = none).
  const [policyOpen, setPolicyOpen] = useState<string>("");
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
          <li key={s.id} className="px-3 py-2 text-xs">
            <div className="flex flex-wrap items-center gap-2">
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
                    onClick={() =>
                      setPolicyOpen((cur) => (cur === s.id ? "" : s.id))
                    }
                  >
                    Policy
                  </Button>
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
            </div>
            {policyOpen === s.id && (
              <PolicyEditor
                schedule={s}
                busy={busy === s.id}
                onSave={async (patch) => {
                  setBusy(s.id);
                  try {
                    await updateTeamSchedule(teamID, s.id, patch);
                    setPolicyOpen("");
                    await reload();
                  } catch (e) {
                    setErr(errorMessage(e));
                  } finally {
                    setBusy(null);
                  }
                }}
                onCancel={() => setPolicyOpen("")}
              />
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

// PolicyEditor edits one schedule's tick policy (pkg/schedgate):
// overlap behavior, optional concurrency cap, and the guard command
// whose stdout becomes vars[guard_var] on fire. Validation of the
// merged row is the server's (400 with a precise message).
function PolicyEditor({
  schedule,
  busy,
  onSave,
  onCancel,
}: {
  schedule: ScheduledBot;
  busy: boolean;
  onSave: (patch: SchedulePatch) => void | Promise<void>;
  onCancel: () => void;
}) {
  const [overlap, setOverlap] = useState<"skip" | "allow">(
    schedule.overlap === "allow" ? "allow" : "skip",
  );
  const [maxConcurrent, setMaxConcurrent] = useState(
    schedule.max_concurrent ? String(schedule.max_concurrent) : "",
  );
  const [guard, setGuard] = useState(schedule.guard ?? "");
  const [guardTimeout, setGuardTimeout] = useState(schedule.guard_timeout ?? "");
  const [guardVar, setGuardVar] = useState(schedule.guard_var ?? "");

  const save = () => {
    const patch: SchedulePatch = {
      overlap,
      max_concurrent:
        overlap === "allow" && maxConcurrent.trim() !== ""
          ? Number(maxConcurrent)
          : 0,
      guard: guard.trim(),
      guard_timeout: guardTimeout.trim(),
      guard_var: guardVar.trim(),
    };
    void onSave(patch);
  };

  return (
    <div className="mt-2 grid gap-2 rounded border border-border-subtle bg-surface-0 p-3 sm:grid-cols-2">
      <label className="grid gap-1">
        <span className="text-fg-muted">Overlap</span>
        <Select
          value={overlap}
          onChange={(e) =>
            setOverlap(e.currentTarget.value === "allow" ? "allow" : "skip")
          }
        >
          <option value="skip">skip — pass the tick while a run is live (audited)</option>
          <option value="allow">allow — fire even with live runs</option>
        </Select>
      </label>
      <label className="grid gap-1">
        <span className="text-fg-muted">Max concurrent (allow only, 0 = unlimited)</span>
        <Input
          type="number"
          min={0}
          value={maxConcurrent}
          disabled={overlap !== "allow"}
          onChange={(e) => setMaxConcurrent(e.currentTarget.value)}
          placeholder="0"
        />
      </label>
      <label className="grid gap-1 sm:col-span-2">
        <span className="text-fg-muted">
          Guard command — exit 0 fires the run (stdout becomes a var), non-zero skips it
        </span>
        <Input
          value={guard}
          onChange={(e) => setGuard(e.currentTarget.value)}
          placeholder="e.g. gh api repos/o/r/pulls --jq 'length > 0'"
          className="font-mono"
        />
      </label>
      <label className="grid gap-1">
        <span className="text-fg-muted">Guard timeout</span>
        <Input
          value={guardTimeout}
          onChange={(e) => setGuardTimeout(e.currentTarget.value)}
          placeholder="30s"
        />
      </label>
      <label className="grid gap-1">
        <span className="text-fg-muted">Guard var (stdout lands here)</span>
        <Input
          value={guardVar}
          onChange={(e) => setGuardVar(e.currentTarget.value)}
          placeholder="guard_output"
          className="font-mono"
        />
      </label>
      <div className="flex items-center gap-2 sm:col-span-2">
        <Button size="sm" disabled={busy} onClick={save}>
          Save policy
        </Button>
        <Button size="sm" variant="ghost" disabled={busy} onClick={onCancel}>
          Cancel
        </Button>
        <span className="text-fg-subtle">
          Tick decisions land on the team audit trail (actions schedule.tick.*).
        </span>
      </div>
    </div>
  );
}
