import { useState } from "react";

import { Dialog } from "@/components/ui/Dialog";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useAsyncAction } from "@/hooks/useAsyncAction";

import {
  createTrigger,
  type SubscriptionInput,
  type TriggerInvocation,
  type TriggerMatcher,
  type TriggerSource,
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

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: () => void;
  /** Pre-fills the Bot field when opened from a bot-scoped surface (the
   *  bot home's "Add trigger…"). Omitted = current behaviour (empty). */
  defaultBotId?: string;
}

export default function NewTriggerDialog({ open, onOpenChange, onCreated, defaultBotId }: Props) {
  const [type, setType] = useState<TriggerType>("board");
  const [botId, setBotId] = useState(defaultBotId ?? "");

  // Re-seed the bot field each time the dialog opens so a stale edit
  // from a previous open doesn't leak into a bot-scoped dialog.
  // Adjust-state-during-render (not an effect) per the React guidance.
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    if (open && defaultBotId) setBotId(defaultBotId);
  }
  const [repo, setRepo] = useState("");
  const [cron, setCron] = useState("0 2 * * *");
  const [kinds, setKinds] = useState("");
  const [states, setStates] = useState("ready");
  const [labels, setLabels] = useState("feature");
  const [authors, setAuthors] = useState("");
  const [argsVar, setArgsVar] = useState("");
  const action = useAsyncAction();

  const meta = TYPE_META[type];

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
    const input: SubscriptionInput = {
      bot_id: botId.trim(),
      repo: repo.trim() || undefined,
      invocation: meta.invocation,
      mode: meta.mode,
      match,
      cron: type === "schedule" ? cron.trim() : undefined,
      args_var: argsVar.trim() || undefined,
      enabled: true,
    };
    const res = await action.run(() => createTrigger(input));
    if (res) onCreated();
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange} title="New trigger" widthClass="max-w-xl">
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
            <Input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="owner/repo" />
          </div>
        </div>

        {type === "schedule" && (
          <div>
            <FieldLabel>Cron (5-field)</FieldLabel>
            <Input value={cron} onChange={(e) => setCron(e.target.value)} placeholder="0 2 * * *" />
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
          <InlineBanner tone="danger" title="Couldn't create trigger">
            {action.error}
          </InlineBanner>
        )}
      </div>

      <div className="flex justify-end gap-2 border-t border-border-default px-4 py-3">
        <Button variant="secondary" size="sm" onClick={() => onOpenChange(false)} disabled={action.busy}>
          Cancel
        </Button>
        <Button variant="primary" size="sm" onClick={() => void submit()} disabled={action.busy}>
          {action.busy ? "Creating…" : "Create trigger"}
        </Button>
      </div>
    </Dialog>
  );
}
