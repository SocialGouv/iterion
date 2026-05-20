import { useMemo, useState } from "react";

import type { BotEntryWithSchema } from "@/api/bots";
import type { VarField } from "@/api/types";
import VarFieldInput, { defaultStringFor } from "@/components/shared/VarFieldInput";
import { isVarMissing, isVarRequired, RequiredPill } from "@/lib/varValidation";

interface Props {
  /** Currently-selected bot's metadata + schema. Null when the form is
   *  in dispatcher-default mode (no bot picked). */
  bot: BotEntryWithSchema | null;
  /** Args currently bound to the ticket. May contain orphan keys not
   *  in the bot's vars schema (e.g. the bot was renamed since save). */
  values: Record<string, string>;
  onChange: (next: Record<string, string>) => void;
}

/** BotArgsForm renders one VarFieldInput per declared workflow var.
 *
 *  Orphan keys — values present in the ticket that are not declared by
 *  the current bot — are surfaced in a collapsible "Unknown args"
 *  section. Never auto-stripped: a bot revert / rename should not
 *  silently lose data; the operator decides.
 */
export function BotArgsForm({ bot, values, onChange }: Props) {
  const [showOrphans, setShowOrphans] = useState(false);
  const fields: VarField[] = useMemo(
    () => (bot?.vars?.fields ?? []) as VarField[],
    [bot],
  );
  const fieldNames = useMemo(
    () => new Set(fields.map((f) => f.name)),
    [fields],
  );
  const orphans = useMemo(() => {
    return Object.entries(values).filter(([k]) => !fieldNames.has(k));
  }, [values, fieldNames]);

  if (!bot) {
    return (
      <p className="text-[11px] text-fg-subtle italic">
        Pick a bot to configure its arguments. With "(dispatcher
        default)" the run uses the workflow + vars defined on the
        dispatcher config.
      </p>
    );
  }

  if (bot.schema_error) {
    return (
      <div className="text-[11px] text-warning-fg space-y-1">
        <div>
          Could not parse the bot's workflow source — args form
          unavailable.
        </div>
        <code className="block bg-surface-1 rounded px-1.5 py-1 break-all">
          {bot.schema_error}
        </code>
      </div>
    );
  }

  const setOne = (name: string, v: string) => {
    onChange({ ...values, [name]: v });
  };
  const deleteOne = (name: string) => {
    const next = { ...values };
    delete next[name];
    onChange(next);
  };

  return (
    <div className="space-y-3">
      {fields.length === 0 ? (
        <p className="text-[11px] text-fg-subtle italic">
          This bot declares no input vars.
        </p>
      ) : (
        <div className="space-y-3">
          {fields.map((f) => {
            const required = isVarRequired(f);
            const value = values[f.name] ?? defaultStringFor(f);
            const invalid = isVarMissing(f, value);
            return (
              <div
                key={f.name}
                className="grid grid-cols-[160px_1fr] gap-3 items-start"
              >
                <label
                  htmlFor={`bot-arg-${f.name}`}
                  className="pt-1"
                >
                  <div className="flex items-baseline gap-2">
                    <span className="text-xs font-medium font-mono">
                      {f.name}
                    </span>
                    {required && <RequiredPill />}
                  </div>
                  <div className="text-[10px] text-fg-subtle">{f.type}</div>
                </label>
                <VarFieldInput
                  field={f}
                  value={value}
                  onChange={(v) => setOne(f.name, v)}
                  required={required}
                  invalid={invalid}
                />
              </div>
            );
          })}
        </div>
      )}

      {orphans.length > 0 && (
        <div className="border border-warning-border rounded-md">
          <button
            type="button"
            onClick={() => setShowOrphans((s) => !s)}
            className="w-full flex items-center justify-between px-2 py-1.5 text-[11px] text-warning-fg hover:bg-surface-1 rounded-md"
          >
            <span>
              Unknown args ({orphans.length}) — not declared by this bot
            </span>
            <span className="text-[10px]">{showOrphans ? "▾" : "▸"}</span>
          </button>
          {showOrphans && (
            <ul className="px-2 py-1 space-y-1">
              {orphans.map(([k, v]) => (
                <li
                  key={k}
                  className="grid grid-cols-[1fr_2fr_auto] gap-2 items-center text-[11px]"
                >
                  <span className="font-mono truncate">{k}</span>
                  <code className="bg-surface-1 rounded px-1.5 py-0.5 truncate">
                    {v}
                  </code>
                  <button
                    type="button"
                    onClick={() => deleteOne(k)}
                    className="text-fg-subtle hover:text-danger"
                    aria-label={`Remove ${k}`}
                  >
                    ×
                  </button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
