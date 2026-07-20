import { useMemo, useState } from "react";
import { useLocation } from "wouter";

import type { BotEntryWithSchema } from "@/api/bots";
import { forgeTeamRepoKey, type ForgeTeamRepo } from "@/api/forgeConnections";
import {
  createTeamSchedule,
  updateTeamSchedule,
  type ScheduledBot,
} from "@/api/schedules";
import CronField from "@/components/shared/CronField";
import SchedulePolicyEditor, {
  policyFieldsFromValue,
  policyValueFromSchedule,
  type SchedulePolicyValue,
} from "@/components/shared/SchedulePolicyEditor";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Combobox, type ComboboxOption } from "@/components/ui/Combobox";
import { Dialog } from "@/components/ui/Dialog";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { useAsyncAction } from "@/hooks/useAsyncAction";

import { buildCreatePayload, intervalSecondsOrError } from "./scheduleModel";

// Create + edit dialogs for the Automations → Schedules tab. Both share
// the CronField (live humanized preview + preset shortcuts) and the
// shared SchedulePolicyEditor; the server validates cron and the merged
// schedgate policy (400 with a precise message surfaced inline).

/** Bot options for a picker: the repo's bound bots when one is chosen,
 *  the full registry otherwise. Personas label, technical id selects. */
function botOptions(
  bots: BotEntryWithSchema[],
  repo: ForgeTeamRepo | null,
): ComboboxOption[] {
  const label = (id: string) =>
    bots.find((b) => b.name === id)?.display_name || id;
  if (repo) {
    return repo.bot_ids.map((id) => ({ value: id, label: label(id), description: id }));
  }
  return bots.map((b) => ({
    value: b.name,
    label: b.display_name || b.name,
    description: b.name,
  }));
}

const NO_REPO = "";

/** The interval + stale-after inputs shown when a schedule is always-on.
 *  Shared by the create and edit dialogs so the two stay in sync. */
function AlwaysOnFields({
  interval,
  staleAfter,
  onInterval,
  onStaleAfter,
  disabled = false,
}: {
  interval: string;
  staleAfter: string;
  onInterval: (v: string) => void;
  onStaleAfter: (v: string) => void;
  disabled?: boolean;
}) {
  return (
    <div className="flex flex-col gap-3">
      <div>
        <FieldLabel>Interval</FieldLabel>
        <Input
          size="sm"
          value={interval}
          onChange={(e) => onInterval(e.target.value)}
          placeholder="30s"
          disabled={disabled}
          className="w-32 font-mono"
        />
        <p className="mt-1 text-xs text-fg-subtle">
          How often to relaunch (min 5s, e.g. 30s / 2m). Sub-minute needs a
          resident scheduler.
        </p>
      </div>
      <div>
        <FieldLabel>Stale after (optional)</FieldLabel>
        <Input
          size="sm"
          value={staleAfter}
          onChange={(e) => onStaleAfter(e.target.value)}
          placeholder="5m"
          disabled={disabled}
          className="w-32 font-mono"
        />
        <p className="mt-1 text-xs text-fg-subtle">
          A run silent past this is treated dead and relaunched (default 5m).
        </p>
      </div>
    </div>
  );
}

export function NewScheduleDialog({
  open,
  onOpenChange,
  onCreated,
  teamID,
  repos,
  bots,
  defaultRepoKey = null,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: () => void;
  teamID: string;
  repos: ForgeTeamRepo[];
  bots: BotEntryWithSchema[];
  /** Pre-selects a repo (forgeTeamRepoKey) when opened from a filtered list. */
  defaultRepoKey?: string | null;
}) {
  // Only repos that carry bound bots can offer a bot choice; a schedule
  // can also live unbound ("no repository").
  const eligibleRepos = useMemo(() => repos.filter((r) => r.bot_ids.length > 0), [repos]);
  const [repoKey, setRepoKey] = useState<string>(defaultRepoKey ?? NO_REPO);
  const [botId, setBotId] = useState("");
  const [cron, setCron] = useState("0 2 * * *");
  const [alwaysOn, setAlwaysOn] = useState(false);
  const [interval, setInterval] = useState("30s");
  const [staleAfter, setStaleAfter] = useState("");
  const [policy, setPolicy] = useState<SchedulePolicyValue>(policyValueFromSchedule());
  const [vars, setVars] = useState<{ key: string; value: string }[]>([]);
  const action = useAsyncAction();
  const [, navigate] = useLocation();

  // Re-seed on each open so a previous draft doesn't leak in.
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    if (open) {
      setRepoKey(
        defaultRepoKey && eligibleRepos.some((r) => forgeTeamRepoKey(r) === defaultRepoKey)
          ? defaultRepoKey
          : NO_REPO,
      );
      setBotId("");
      setCron("0 2 * * *");
      setAlwaysOn(false);
      setInterval("30s");
      setStaleAfter("");
      setPolicy(policyValueFromSchedule());
      setVars([]);
      action.clearError();
    }
  }

  const repo = eligibleRepos.find((r) => forgeTeamRepoKey(r) === repoKey) ?? null;
  const options = useMemo(() => botOptions(bots, repo), [bots, repo]);

  async function submit() {
    if (!botId.trim()) {
      action.setError("Bot is required.");
      return;
    }
    let intervalSeconds = 0;
    if (alwaysOn) {
      const iv = intervalSecondsOrError(interval);
      if ("error" in iv) {
        action.setError(iv.error);
        return;
      }
      intervalSeconds = iv.seconds;
    } else if (!cron.trim()) {
      action.setError("Cron is required.");
      return;
    }
    const res = await action.run(() =>
      createTeamSchedule(
        teamID,
        buildCreatePayload({
          botId,
          cron,
          repo,
          policy,
          vars: Object.fromEntries(vars.map((r) => [r.key, r.value])),
          alwaysOn,
          intervalSeconds,
          staleAfter,
        }),
      ),
    );
    if (res) onCreated();
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange} title="New schedule" widthClass="max-w-xl">
      <div className="flex flex-col gap-3 px-4 py-3">
        <div>
          <FieldLabel>Repository</FieldLabel>
          <Select
            value={repoKey}
            onChange={(e) => {
              setRepoKey(e.currentTarget.value);
              // A repo change invalidates a bot picked from the other list.
              setBotId("");
            }}
          >
            <option value={NO_REPO}>No repository — team-level schedule</option>
            {eligibleRepos.map((r) => (
              <option key={forgeTeamRepoKey(r)} value={forgeTeamRepoKey(r)}>
                {r.repo_full_name} ({r.provider})
              </option>
            ))}
          </Select>
          {eligibleRepos.length === 0 && (
            <p className="mt-1 text-xs text-fg-muted">
              No connected repo has bots enabled yet —{" "}
              <button
                type="button"
                className="text-accent hover:underline"
                onClick={() => navigate("/integrations/connect")}
              >
                connect a repository
              </button>{" "}
              or schedule against the full bot registry.
            </p>
          )}
        </div>

        <div>
          <FieldLabel>Bot</FieldLabel>
          <Combobox
            value={botId}
            options={options}
            placeholder={repo ? "Pick one of the repo's bots" : "Pick a bot"}
            onChange={(v) => setBotId(v)}
            freeSolo
          />
          {repo && (
            <p className="mt-1 text-xs text-fg-muted">
              Bots enabled on {repo.repo_full_name}.
            </p>
          )}
        </div>

        <Checkbox
          checked={alwaysOn}
          onChange={(e) => setAlwaysOn(e.currentTarget.checked)}
          label={
            <span>
              Always-on
              <span className="ml-1 text-xs text-fg-subtle">
                — keep this bot running: relaunch on an interval, at most one live at a time
              </span>
            </span>
          }
        />

        {alwaysOn ? (
          <AlwaysOnFields
            interval={interval}
            staleAfter={staleAfter}
            onInterval={setInterval}
            onStaleAfter={setStaleAfter}
          />
        ) : (
          <CronField value={cron} onChange={setCron} />
        )}

        <div>
          <FieldLabel>Variables (optional)</FieldLabel>
          <p className="mb-1 text-caption text-fg-subtle">
            Passed to the bot on each fire — e.g. a feed-watch per-category
            digest needs <span className="font-mono">mode=digest</span> and{" "}
            <span className="font-mono">category=a11y</span>.
          </p>
          <div className="flex flex-col gap-1.5">
            {vars.map((row, i) => (
              <div key={i} className="flex items-center gap-2">
                <Input
                  size="sm"
                  placeholder="name"
                  value={row.key}
                  onChange={(e) =>
                    setVars((rows) =>
                      rows.map((r, j) =>
                        j === i ? { ...r, key: e.target.value } : r,
                      ),
                    )
                  }
                  className="w-32 font-mono"
                />
                <span className="text-fg-subtle">=</span>
                <Input
                  size="sm"
                  placeholder="value"
                  value={row.value}
                  onChange={(e) =>
                    setVars((rows) =>
                      rows.map((r, j) =>
                        j === i ? { ...r, value: e.target.value } : r,
                      ),
                    )
                  }
                  className="flex-1 font-mono"
                />
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() =>
                    setVars((rows) => rows.filter((_, j) => j !== i))
                  }
                  aria-label="Remove variable"
                >
                  ✕
                </Button>
              </div>
            ))}
            <div>
              <Button
                variant="secondary"
                size="sm"
                onClick={() =>
                  setVars((rows) => [...rows, { key: "", value: "" }])
                }
              >
                + Add variable
              </Button>
            </div>
          </div>
        </div>

        {!alwaysOn && (
          <div>
            <FieldLabel>Tick policy</FieldLabel>
            <SchedulePolicyEditor value={policy} onChange={setPolicy} disabled={action.busy} />
          </div>
        )}

        {action.error && (
          <InlineBanner tone="danger" title="Couldn't create schedule">
            {action.error}
          </InlineBanner>
        )}
      </div>

      <div className="flex justify-end gap-2 border-t border-border-default px-4 py-3">
        <Button variant="secondary" size="sm" onClick={() => onOpenChange(false)} disabled={action.busy}>
          Cancel
        </Button>
        <Button variant="primary" size="sm" onClick={() => void submit()} disabled={action.busy}>
          {action.busy ? "Creating…" : "Create schedule"}
        </Button>
      </div>
    </Dialog>
  );
}

export function EditScheduleDialog({
  teamID,
  schedule,
  onOpenChange,
  onSaved,
}: {
  teamID: string;
  /** The schedule under edit — null keeps the dialog closed. */
  schedule: ScheduledBot | null;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const [cron, setCron] = useState("");
  const [interval, setInterval] = useState("30s");
  const [staleAfter, setStaleAfter] = useState("");
  const [policy, setPolicy] = useState<SchedulePolicyValue>(policyValueFromSchedule());
  const action = useAsyncAction();

  const alwaysOn = (schedule?.interval_seconds ?? 0) > 0 || schedule?.overlap === "keepalive";

  // Re-seed the draft whenever a (different) schedule is opened.
  const [seededFor, setSeededFor] = useState<string | null>(null);
  if (schedule && schedule.id !== seededFor) {
    setSeededFor(schedule.id);
    setCron(schedule.cron);
    setInterval(
      schedule.interval_seconds ? `${schedule.interval_seconds}s` : "30s",
    );
    setStaleAfter(schedule.stale_after ?? "");
    setPolicy(policyValueFromSchedule(schedule));
    action.clearError();
  }
  if (!schedule && seededFor !== null) setSeededFor(null);

  async function submit() {
    if (!schedule) return;
    if (alwaysOn) {
      const iv = intervalSecondsOrError(interval);
      if ("error" in iv) {
        action.setError(iv.error);
        return;
      }
      const res = await action.run(() =>
        updateTeamSchedule(teamID, schedule.id, {
          interval_seconds: iv.seconds,
          stale_after: staleAfter.trim() || undefined,
        }),
      );
      if (res) onSaved();
      return;
    }
    if (!cron.trim()) {
      action.setError("Cron is required.");
      return;
    }
    const res = await action.run(() =>
      updateTeamSchedule(teamID, schedule.id, {
        cron: cron.trim(),
        ...policyFieldsFromValue(policy),
      }),
    );
    if (res) onSaved();
  }

  return (
    <Dialog
      open={schedule !== null}
      onOpenChange={onOpenChange}
      title={schedule ? `Edit schedule — ${schedule.bot_id}` : "Edit schedule"}
      widthClass="max-w-xl"
    >
      <div className="flex flex-col gap-3 px-4 py-3">
        {alwaysOn ? (
          <div className="flex flex-col gap-3">
            <div className="rounded-md bg-surface-sunken px-3 py-2 text-xs text-fg-subtle">
              Always-on schedule — relaunches on an interval with at most one live
              run at a time.
            </div>
            <AlwaysOnFields
              interval={interval}
              staleAfter={staleAfter}
              onInterval={setInterval}
              onStaleAfter={setStaleAfter}
              disabled={action.busy}
            />
          </div>
        ) : (
          <>
            <CronField value={cron} onChange={setCron} disabled={action.busy} />
            <div>
              <FieldLabel>Tick policy</FieldLabel>
              <SchedulePolicyEditor value={policy} onChange={setPolicy} disabled={action.busy} />
            </div>
          </>
        )}

        {action.error && (
          <InlineBanner tone="danger" title="Couldn't save schedule">
            {action.error}
          </InlineBanner>
        )}
      </div>

      <div className="flex justify-end gap-2 border-t border-border-default px-4 py-3">
        <Button variant="secondary" size="sm" onClick={() => onOpenChange(false)} disabled={action.busy}>
          Cancel
        </Button>
        <Button variant="primary" size="sm" onClick={() => void submit()} disabled={action.busy}>
          {action.busy ? "Saving…" : "Save changes"}
        </Button>
      </div>
    </Dialog>
  );
}
