import { type BotEntryWithSchema } from "@/api/bots";

import { BotArgsForm } from "../BotArgsForm";
import { BotPicker } from "../BotPicker";
import { Field } from "./Field";

interface BotTabProps {
  bots: BotEntryWithSchema[] | null;
  botsError: string | null;
  bot: string;
  setBot: (v: string) => void;
  botArgs: Record<string, string>;
  setBotArgs: (next: Record<string, string>) => void;
  selectedBot: BotEntryWithSchema | null;
}

export function BotTab({
  bots,
  botsError,
  bot,
  setBot,
  botArgs,
  setBotArgs,
  selectedBot,
}: BotTabProps) {
  return (
    <div className="space-y-3 py-3">
      <Field label="Bot">
        {botsError ? (
          <div className="text-xs text-warning-fg">
            Could not load bots: {botsError}
          </div>
        ) : bots == null ? (
          <div className="text-xs text-fg-subtle italic">Loading bots…</div>
        ) : bots.length === 0 ? (
          <div className="text-xs text-fg-subtle italic">
            No bots discovered. Configure <code>--bots-path</code> on
            the studio or set <code>bots.paths</code> on the dispatcher
            config.
          </div>
        ) : (
          <BotPicker value={bot} bots={bots} onChange={setBot} />
        )}
        <p className="text-micro text-fg-subtle mt-1">
          When set, this bot overrides the dispatcher's per-assignee or
          global workflow selection for this ticket.
        </p>
      </Field>

      <Field label="Arguments">
        <BotArgsForm
          bot={bot ? selectedBot : null}
          values={botArgs}
          onChange={setBotArgs}
        />
      </Field>
    </div>
  );
}
