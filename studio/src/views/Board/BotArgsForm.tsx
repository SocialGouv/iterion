import { useMemo, useState } from "react";

import type { BotEntryWithSchema } from "@/api/bots";
import type { VarField } from "@/api/types";
import { Input } from "@/components/ui/Input";
import VarFieldInput, { defaultStringFor } from "@/components/shared/VarFieldInput";
import { isVarMissing, isVarRequired, RequiredPill } from "@/lib/varValidation";

interface Props {
  /** Currently-selected bot's metadata + schema. Null when no bot is
   *  picked — the form still lets the operator add raw key/value
   *  entries (overrides flow to the dispatcher's templated vars). */
  bot: BotEntryWithSchema | null;
  /** Args currently bound to the ticket. May contain extra keys not
   *  in the bot's vars schema (the operator added them manually, or
   *  the bot was renamed since save). */
  values: Record<string, string>;
  onChange: (next: Record<string, string>) => void;
}

/** BotArgsForm has two parts:
 *
 *  1. Schema-driven typed fields — one VarFieldInput per declared
 *     workflow var of the picked bot. Surfaces required pills and
 *     missing-value diagnostics.
 *
 *  2. Custom args — an editable list of arbitrary key/value entries.
 *     Holds both the "extra" keys not declared by the bot's schema
 *     (renamed bot, untyped overrides) and the operator's hand-added
 *     entries. Always shown, with a "+ Add arg" button to append.
 *     This is the path used when a bot has no vars yet (or no bot
 *     is picked at all) but the operator still wants to set custom
 *     overrides for the dispatcher's vars templates.
 *
 *  Custom args are never auto-stripped: a bot revert/rename should
 *  not silently lose data. The operator decides what to remove.
 */
export function BotArgsForm({ bot, values, onChange }: Props) {
  const fields: VarField[] = useMemo(
    () => (bot?.vars?.fields ?? []) as VarField[],
    [bot],
  );
  const fieldNames = useMemo(
    () => new Set(fields.map((f) => f.name)),
    [fields],
  );
  // Custom args = every entry in `values` that is not in the schema.
  // Sorted by key for stable order across re-renders.
  const customEntries = useMemo(
    () =>
      Object.entries(values)
        .filter(([k]) => !fieldNames.has(k))
        .sort((a, b) => a[0].localeCompare(b[0])),
    [values, fieldNames],
  );

  // Draft for the "Add arg" row. Committed to `values` only when the
  // key is non-empty AND not already present. Keeps the form free of
  // ghost-empty rows on every keystroke.
  const [draftKey, setDraftKey] = useState("");
  const [draftValue, setDraftValue] = useState("");

  const setOne = (name: string, v: string) => {
    onChange({ ...values, [name]: v });
  };
  const deleteOne = (name: string) => {
    const next = { ...values };
    delete next[name];
    onChange(next);
  };
  const renameCustom = (oldKey: string, newKey: string) => {
    if (newKey === oldKey) return;
    const trimmed = newKey.trim();
    if (!trimmed) return;
    if (trimmed in values && trimmed !== oldKey) return; // collision: ignore
    const next: Record<string, string> = {};
    for (const [k, v] of Object.entries(values)) {
      if (k === oldKey) next[trimmed] = v;
      else next[k] = v;
    }
    onChange(next);
  };
  const commitDraft = () => {
    const k = draftKey.trim();
    if (!k) return;
    if (k in values) {
      setDraftKey("");
      setDraftValue("");
      return;
    }
    onChange({ ...values, [k]: draftValue });
    setDraftKey("");
    setDraftValue("");
  };

  return (
    <div className="space-y-3">
      {bot?.schema_error && (
        <div className="text-[11px] text-warning-fg space-y-1">
          <div>
            Schema unavailable for this bot — the workflow file failed to parse.
            Custom args below still flow to the dispatcher.
          </div>
          <code className="block bg-surface-1 rounded px-1.5 py-1 break-all">
            {bot.schema_error}
          </code>
        </div>
      )}

      {/* Schema-driven fields */}
      {bot && !bot.schema_error && fields.length > 0 && (
        <div className="space-y-3">
          <div className="text-[10px] uppercase tracking-wide text-fg-subtle">
            Schema-declared vars
          </div>
          {fields.map((f) => {
            const required = isVarRequired(f);
            const value = values[f.name] ?? defaultStringFor(f);
            const invalid = isVarMissing(f, value);
            return (
              <div
                key={f.name}
                className="grid grid-cols-[160px_1fr] gap-3 items-start"
              >
                <label htmlFor={`bot-arg-${f.name}`} className="pt-1">
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

      {bot && !bot.schema_error && fields.length === 0 && (
        <p className="text-[11px] text-fg-subtle italic">
          This bot declares no input vars. Use the custom args section
          below to set overrides; they flow to the dispatcher&apos;s vars
          templates at launch.
        </p>
      )}

      {/* Custom args — always rendered. */}
      <div className="space-y-2">
        <div className="text-[10px] uppercase tracking-wide text-fg-subtle">
          Custom args
          {customEntries.length > 0 && bot && !bot.schema_error && fields.length > 0 && (
            <span className="ml-1 normal-case text-warning-fg">
              ({customEntries.length} override{customEntries.length === 1 ? "" : "s"}{" "}
              not declared by this bot)
            </span>
          )}
        </div>
        {customEntries.length === 0 && (
          <p className="text-[11px] text-fg-subtle italic">
            No custom args yet. The row below adds a key/value override that
            the dispatcher passes to the bot&apos;s vars.
          </p>
        )}
        {customEntries.map(([k, v]) => (
          <div
            key={k}
            className="grid grid-cols-[160px_1fr_auto] gap-2 items-center"
          >
            <Input
              value={k}
              onChange={(e) => renameCustom(k, e.target.value)}
              size="sm"
              className="font-mono"
              aria-label={`Key for ${k}`}
            />
            <Input
              value={v}
              onChange={(e) => setOne(k, e.target.value)}
              size="sm"
              aria-label={`Value for ${k}`}
            />
            <button
              type="button"
              onClick={() => deleteOne(k)}
              className="text-fg-subtle hover:text-danger px-1"
              aria-label={`Remove ${k}`}
            >
              ×
            </button>
          </div>
        ))}
        {/* Add-row */}
        <div className="grid grid-cols-[160px_1fr_auto] gap-2 items-center">
          <Input
            value={draftKey}
            onChange={(e) => setDraftKey(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                commitDraft();
              }
            }}
            placeholder="key"
            size="sm"
            className="font-mono"
            aria-label="New arg key"
          />
          <Input
            value={draftValue}
            onChange={(e) => setDraftValue(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                commitDraft();
              }
            }}
            placeholder="value"
            size="sm"
            aria-label="New arg value"
          />
          <button
            type="button"
            onClick={commitDraft}
            disabled={!draftKey.trim() || draftKey.trim() in values}
            className="text-xs px-2 py-1 rounded border border-border-default hover:bg-surface-2 disabled:opacity-40 disabled:cursor-not-allowed"
            title={
              draftKey.trim() in values
                ? "Key already set above"
                : "Add this key/value entry"
            }
          >
            + Add
          </button>
        </div>
      </div>
    </div>
  );
}
