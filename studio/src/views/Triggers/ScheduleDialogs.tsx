import { useMemo, useState } from "react";
import { useLocation } from "wouter";

import type { BotEntryWithSchema } from "@/api/bots";
import { forgeTeamRepoKey, type ForgeTeamRepo } from "@/api/forgeConnections";
import {
  createTeamSchedule,
  updateTeamSchedule,
  type ScheduledBot,
} from "@/api/schedules";
import SchedulePolicyEditor, {
  policyFieldsFromValue,
  policyValueFromSchedule,
  type SchedulePolicyValue,
} from "@/components/shared/SchedulePolicyEditor";
import { Button } from "@/components/ui/Button";
import { Combobox, type ComboboxOption } from "@/components/ui/Combobox";
import { Dialog } from "@/components/ui/Dialog";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { Input } from "@/components/ui/Input";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Select } from "@/components/ui/Select";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { humanizeCron } from "@/lib/humanizeCron";

import { buildCreatePayload } from "./scheduleModel";

// Create + edit dialogs for the Automations → Schedules tab. Both share
// the CronField (live humanized preview + preset shortcuts) and the
// shared SchedulePolicyEditor; the server validates cron and the merged
// schedgate policy (400 with a precise message surfaced inline).

const CRON_PRESETS = [
  { label: "Hourly", cron: "0 * * * *" },
  { label: "Daily 02:00", cron: "0 2 * * *" },
  { label: "Weekly Mon 02:00", cron: "0 2 * * 1" },
];

function CronField({
  value,
  onChange,
  disabled = false,
}: {
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
}) {
  const human = humanizeCron(value);
  return (
    <div>
      <FieldLabel>Cron (5-field, UTC — or prefix CRON_TZ=&lt;zone&gt;)</FieldLabel>
      <Input
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.currentTarget.value)}
        placeholder="0 2 * * *"
        className="font-mono"
      />
      <div className="mt-1 flex flex-wrap items-center gap-1">
        {CRON_PRESETS.map((p) => (
          <Button
            key={p.cron}
            variant="ghost"
            size="sm"
            disabled={disabled}
            onClick={() => onChange(p.cron)}
          >
            {p.label}
          </Button>
        ))}
      </div>
      <p className="mt-1 min-h-4 text-xs text-fg-muted" aria-live="polite">
        {human ??
          (value.trim()
            ? "Unrecognized shape — the raw expression is used as-is."
            : "")}
      </p>
    </div>
  );
}

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
  const [policy, setPolicy] = useState<SchedulePolicyValue>(policyValueFromSchedule());
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
      setPolicy(policyValueFromSchedule());
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
    if (!cron.trim()) {
      action.setError("Cron is required.");
      return;
    }
    const res = await action.run(() =>
      createTeamSchedule(teamID, buildCreatePayload({ botId, cron, repo, policy })),
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

        <CronField value={cron} onChange={setCron} />

        <div>
          <FieldLabel>Tick policy</FieldLabel>
          <SchedulePolicyEditor value={policy} onChange={setPolicy} disabled={action.busy} />
        </div>

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
  const [policy, setPolicy] = useState<SchedulePolicyValue>(policyValueFromSchedule());
  const action = useAsyncAction();

  // Re-seed the draft whenever a (different) schedule is opened.
  const [seededFor, setSeededFor] = useState<string | null>(null);
  if (schedule && schedule.id !== seededFor) {
    setSeededFor(schedule.id);
    setCron(schedule.cron);
    setPolicy(policyValueFromSchedule(schedule));
    action.clearError();
  }
  if (!schedule && seededFor !== null) setSeededFor(null);

  async function submit() {
    if (!schedule) return;
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
        <CronField value={cron} onChange={setCron} disabled={action.busy} />

        <div>
          <FieldLabel>Tick policy</FieldLabel>
          <SchedulePolicyEditor value={policy} onChange={setPolicy} disabled={action.busy} />
        </div>

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
