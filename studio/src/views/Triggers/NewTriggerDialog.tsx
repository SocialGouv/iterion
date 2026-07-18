import { useMemo, useState } from "react";

import { Dialog } from "@/components/ui/Dialog";
import { Button } from "@/components/ui/Button";
import { Combobox, type ComboboxOption } from "@/components/ui/Combobox";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useAsyncAction } from "@/hooks/useAsyncAction";

import {
  createTrigger,
  updateTrigger,
  toSubscriptionInput,
  type SubscriptionInput,
  type TriggerInvocation,
  type TriggerMatcher,
  type TriggerSource,
  type TriggerSubscription,
} from "@/api/triggers";

// TriggerType is the operator-facing choice that drives the source + sensible
// invocation/mode defaults + which match fields are relevant.
type TriggerType = "board" | "schedule" | "run" | "forge" | "custom";

const TYPE_META: Record<
  TriggerType,
  { label: string; source: TriggerSource; invocation?: TriggerInvocation; mode: "direct" | "board"; hint: string }
> = {
  board: { label: "Board card", source: "board", invocation: "board", mode: "board", hint: "Fires when a native-board card transitions (promotes the card; the dispatcher claims it)." },
  schedule: { label: "Schedule (cron)", source: "schedule", invocation: "schedule", mode: "direct", hint: "Fires on a cron cadence (local scope)." },
  run: { label: "Run finished", source: "run", mode: "direct", hint: "Chains a bot after another run finishes/fails (author = the upstream bot id)." },
  forge: { label: "Forge event", source: "forge", invocation: "forge", mode: "direct", hint: "Fires on a git-forge webhook event (PR/MR/comment)." },
  custom: { label: "Custom integration", source: "custom", mode: "direct", hint: "Fires on a POST /api/v1/triggers/emit event of the given kind." },
};

// csv splits a comma/space list into a trimmed array (empty → undefined so the
// matcher dimension stays "match any").
function csv(s: string): string[] | undefined {
  const out = s
    .split(/[,\s]+/)
    .map((x) => x.trim())
    .filter(Boolean);
  return out.length ? out : undefined;
}

// typeFromSubscription recovers the operator-facing type from a stored
// subscription: the matcher's pinned source when it maps to a type, else the
// invocation kind, else "custom".
function typeFromSubscription(sub: TriggerSubscription): TriggerType {
  const s = sub.match?.sources?.[0];
  if (s && s in TYPE_META) return s as TriggerType;
  if (sub.invocation === "schedule" || sub.invocation === "board" || sub.invocation === "forge") {
    return sub.invocation;
  }
  return "custom";
}

const DEFAULT_CRON = "0 2 * * *";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Called after a successful create OR edit save. */
  onCreated: () => void;
  /** Edit mode: the subscription under edit. The dialog re-seeds from it on
   *  open and saves via PUT instead of POST. Null/omitted = create mode. */
  editing?: TriggerSubscription | null;
  /** Pre-fills the Bot field when opened from a bot-scoped surface (the
   *  bot home's "Add trigger…"). Omitted = current behaviour (empty). */
  defaultBotId?: string;
  /** Pre-fills the Repo field with an explicit owner/repo slug. When
   *  omitted the dialog falls back to the sidebar's active repo in cloud
   *  mode. Free text stays accepted for repos not yet connected. */
  defaultRepo?: string;
}

export default function NewTriggerDialog({
  open,
  onOpenChange,
  onCreated,
  editing = null,
  defaultBotId,
  defaultRepo,
}: Props) {
  const [type, setType] = useState<TriggerType>("board");
  const [botId, setBotId] = useState(defaultBotId ?? "");
  // Repo picker is fed by the connected-repos list the sidebar switcher
  // uses; free text stays accepted for repos not yet connected on the
  // team. Fallback prefill: caller's defaultRepo, else the active repo.
  const { activeRepo, repos: connectedRepos, enabled: repoScopeEnabled } = useActiveRepo();
  const initialRepo = defaultRepo ?? (repoScopeEnabled ? activeRepo?.repo_full_name ?? "" : "");
  const [repo, setRepo] = useState(initialRepo);
  const [cron, setCron] = useState(DEFAULT_CRON);
  const [kinds, setKinds] = useState("");
  const [states, setStates] = useState("ready");
  const [labels, setLabels] = useState("feature");
  const [authors, setAuthors] = useState("");
  const [argsVar, setArgsVar] = useState("");
  const action = useAsyncAction();

  // Re-seed the whole draft each time the dialog opens (or the edited
  // subscription changes) so a stale draft from a previous open never
  // leaks. Adjust-state-during-render (not an effect) per the React
  // guidance — same pattern as EditScheduleDialog's seededFor.
  const seedKey = open ? (editing ? `edit:${editing.id}` : "create") : null;
  const [seededFor, setSeededFor] = useState<string | null>(null);
  if (seedKey !== seededFor) {
    setSeededFor(seedKey);
    if (seedKey) {
      if (editing) {
        setType(typeFromSubscription(editing));
        setBotId(editing.bot_id);
        setRepo(editing.repo ?? "");
        setCron(editing.cron || DEFAULT_CRON);
        setKinds(editing.match?.kinds?.join(", ") ?? "");
        setStates(editing.match?.subject_states?.join(", ") ?? "");
        setLabels(editing.match?.labels?.join(", ") ?? "");
        setAuthors(editing.match?.authors?.join(", ") ?? "");
        setArgsVar(editing.args_var ?? "");
      } else {
        setType("board");
        setBotId(defaultBotId ?? "");
        setRepo(initialRepo);
        setCron(DEFAULT_CRON);
        setKinds("");
        setStates("ready");
        setLabels("feature");
        setAuthors("");
        setArgsVar("");
      }
      action.clearError();
    }
  }

  const meta = TYPE_META[type];

  const repoOptions = useMemo<ComboboxOption[]>(
    () =>
      connectedRepos.map((r) => ({
        value: r.repo_full_name,
        label: r.repo_full_name,
        description: r.provider,
      })),
    [connectedRepos],
  );

  async function submit() {
    if (!botId.trim()) {
      action.setError("Bot is required.");
      return;
    }
    const match: TriggerMatcher = { sources: [meta.source] };
    if (type === "board") {
      match.subject_states = csv(states);
      match.labels = csv(labels);
      if (csv(kinds)) match.kinds = csv(kinds);
    } else if (type === "run" || type === "forge" || type === "custom") {
      match.kinds = csv(kinds);
      if (type === "run" || type === "forge") match.authors = csv(authors);
    }
    // Edit mode starts from the full projection of the current subscription
    // (PUT replaces every request field — vars + schedgate policy would be
    // cleared otherwise), then overlays the dialog fields.
    const input: SubscriptionInput = {
      ...(editing ? toSubscriptionInput(editing) : {}),
      bot_id: botId.trim(),
      repo: repo.trim() || undefined,
      invocation: meta.invocation,
      mode: meta.mode,
      match,
      cron: type === "schedule" ? cron.trim() : undefined,
      args_var: argsVar.trim() || undefined,
      enabled: editing ? editing.enabled : true,
    };
    const res = await action.run(() =>
      editing ? updateTrigger(editing.id, input) : createTrigger(input),
    );
    if (res) onCreated();
  }

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={editing ? "Edit trigger" : "New trigger"}
      widthClass="max-w-xl"
    >
      <div className="flex flex-col gap-3 px-4 py-3">
        <div>
          <FieldLabel>Trigger type</FieldLabel>
          <Select value={type} onChange={(e) => setType(e.target.value as TriggerType)}>
            {Object.entries(TYPE_META).map(([k, v]) => (
              <option key={k} value={k}>
                {v.label}
              </option>
            ))}
          </Select>
          <p className="mt-1 text-xs text-fg-muted">{meta.hint}</p>
        </div>

        <div className="grid grid-cols-2 gap-3">
          <div>
            <FieldLabel>Bot</FieldLabel>
            <Input value={botId} onChange={(e) => setBotId(e.target.value)} placeholder="feature-dev" />
          </div>
          <div>
            <FieldLabel>Repo (optional)</FieldLabel>
            <Combobox
              value={repo}
              options={repoOptions}
              placeholder="owner/repo"
              emptyLabel="Any repository"
              onChange={(v) => setRepo(v)}
              freeSolo
            />
          </div>
        </div>

        {type === "schedule" && (
          <div>
            <FieldLabel>Cron (5-field)</FieldLabel>
            <Input value={cron} onChange={(e) => setCron(e.target.value)} placeholder={DEFAULT_CRON} />
          </div>
        )}

        {type === "board" && (
          <div className="grid grid-cols-2 gap-3">
            <div>
              <FieldLabel>Enters state(s)</FieldLabel>
              <Input value={states} onChange={(e) => setStates(e.target.value)} placeholder="ready" />
            </div>
            <div>
              <FieldLabel>With all label(s)</FieldLabel>
              <Input value={labels} onChange={(e) => setLabels(e.target.value)} placeholder="feature" />
            </div>
          </div>
        )}

        {(type === "run" || type === "forge" || type === "custom") && (
          <div>
            <FieldLabel>Event kind(s) {type === "run" ? "(run.finished, run.failed)" : ""}</FieldLabel>
            <Input
              value={kinds}
              onChange={(e) => setKinds(e.target.value)}
              placeholder={type === "run" ? "run.finished" : type === "forge" ? "pull_request" : "ci.done"}
            />
          </div>
        )}

        {(type === "run" || type === "forge") && (
          <div>
            <FieldLabel>{type === "run" ? "Upstream bot (author)" : "Author(s)"} (optional)</FieldLabel>
            <Input value={authors} onChange={(e) => setAuthors(e.target.value)} placeholder={type === "run" ? "feature-dev" : "dependabot[bot]"} />
          </div>
        )}

        <div>
          <FieldLabel>Args var (optional)</FieldLabel>
          <Input value={argsVar} onChange={(e) => setArgsVar(e.target.value)} placeholder="feature_prompt" />
          <p className="mt-1 text-xs text-fg-muted">
            Workflow var that receives the event payload (issue title+body, comment args).
          </p>
        </div>

        {action.error && (
          <InlineBanner tone="danger" title={editing ? "Couldn't save trigger" : "Couldn't create trigger"}>
            {action.error}
          </InlineBanner>
        )}
      </div>

      <div className="flex justify-end gap-2 border-t border-border-default px-4 py-3">
        <Button variant="secondary" size="sm" onClick={() => onOpenChange(false)} disabled={action.busy}>
          Cancel
        </Button>
        <Button variant="primary" size="sm" onClick={() => void submit()} disabled={action.busy}>
          {editing
            ? action.busy
              ? "Saving…"
              : "Save changes"
            : action.busy
              ? "Creating…"
              : "Create trigger"}
        </Button>
      </div>
    </Dialog>
  );
}
