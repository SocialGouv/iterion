import { useEffect, useMemo, useState } from "react";

import { listBots, type BotEntryWithSchema } from "@/api/bots";
import type { NativeBoard, NativeIssue } from "@/api/native";
import { Dialog } from "@/components/ui/Dialog";
import { Input } from "@/components/ui/Input";
import { MarkdownPreview } from "@/components/ui/MarkdownPreview";
import { Select } from "@/components/ui/Select";
import { Tabs } from "@/components/ui/Tabs";
import { TagInput } from "@/components/ui/TagInput";
import VarFieldInput, { defaultStringFor } from "@/components/shared/VarFieldInput";
import { isVarMissing, RequiredPill } from "@/lib/varValidation";

import { BotArgsForm } from "./BotArgsForm";
import { BotPicker } from "./BotPicker";

void VarFieldInput; // re-exported through BotArgsForm; keep import path stable

interface Props {
  board: NativeBoard;
  initial: NativeIssue | null;
  onSubmit: (input: Partial<NativeIssue>) => Promise<void> | void;
  onClose: () => void;
  onDelete?: () => void;
}

export default function IssueModal({ board, initial, onSubmit, onClose, onDelete }: Props) {
  const [tab, setTab] = useState<"ticket" | "bot">("ticket");
  const [title, setTitle] = useState(initial?.title ?? "");
  const [body, setBody] = useState(initial?.body ?? "");
  const [state, setState] = useState(initial?.state ?? board.states[0]?.name ?? "");
  const [labels, setLabels] = useState<string[]>(initial?.labels ?? []);
  const [priority, setPriority] = useState(initial?.priority ?? 0);
  const [assignee, setAssignee] = useState(initial?.assignee ?? "");
  const [bot, setBot] = useState(initial?.bot ?? "");
  const [botArgs, setBotArgs] = useState<Record<string, string>>(
    initial?.bot_args ?? {},
  );
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const [fields, setFields] = useState<Record<string, string>>(() => {
    const out: Record<string, string> = {};
    for (const f of board.fields ?? []) {
      const v = initial?.fields?.[f.name];
      out[f.name] = v == null ? "" : String(v);
    }
    return out;
  });

  // Bots catalog. Fetched once when the modal opens. Loading + error
  // surface separately so the Bot tab degrades gracefully.
  const [bots, setBots] = useState<BotEntryWithSchema[] | null>(null);
  const [botsError, setBotsError] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    setBots(null);
    setBotsError(null);
    listBots()
      .then((items) => {
        if (!cancelled) setBots(items);
      })
      .catch((err) => {
        if (!cancelled) setBotsError((err as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Re-seed when the parent swaps to a different issue without unmount.
  useEffect(() => {
    setTab("ticket");
    setTitle(initial?.title ?? "");
    setBody(initial?.body ?? "");
    setState(initial?.state ?? board.states[0]?.name ?? "");
    setLabels(initial?.labels ?? []);
    setPriority(initial?.priority ?? 0);
    setAssignee(initial?.assignee ?? "");
    setBot(initial?.bot ?? "");
    setBotArgs(initial?.bot_args ?? {});
    const out: Record<string, string> = {};
    for (const f of board.fields ?? []) {
      const v = initial?.fields?.[f.name];
      out[f.name] = v == null ? "" : String(v);
    }
    setFields(out);
  }, [initial, board]);

  const selectedBot: BotEntryWithSchema | null = useMemo(() => {
    if (!bot || !bots) return null;
    return bots.find((b) => b.name === bot) ?? null;
  }, [bot, bots]);

  const botRequiredMissing = useMemo(() => {
    if (!selectedBot?.vars?.fields) return false;
    return selectedBot.vars.fields.some((f) =>
      isVarMissing(f, botArgs[f.name] ?? defaultStringFor(f)),
    );
  }, [selectedBot, botArgs]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (submitting) return;
    if (botRequiredMissing) {
      setTab("bot");
      setSubmitError("Required bot arguments are missing.");
      return;
    }
    const out: Partial<NativeIssue> = {
      title: title.trim(),
      body: body.trim(),
      state,
      labels,
      priority,
      assignee: assignee.trim() || undefined,
      bot: bot.trim() || undefined,
      bot_args: Object.keys(botArgs).length > 0 ? botArgs : undefined,
    };
    const typedFields = coerceFields(board, fields);
    if (Object.keys(typedFields).length > 0) {
      out.fields = typedFields;
    }
    setSubmitting(true);
    setSubmitError(null);
    try {
      await onSubmit(out);
    } catch (err) {
      setSubmitError((err as Error).message || "Submit failed");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={initial ? "Edit issue" : "New issue"}
      widthClass="max-w-[42rem]"
    >
      <form onSubmit={submit} className="max-h-[80vh] overflow-auto">
        <div className="px-4 pt-2">
          <Tabs
            value={tab}
            onValueChange={(v) => setTab(v as "ticket" | "bot")}
            items={[
              { value: "ticket", label: "Ticket" },
              {
                value: "bot",
                label: (
                  <span className="inline-flex items-center gap-1">
                    Bot
                    {bot && (
                      <span className="text-[10px] font-mono bg-accent/15 text-accent rounded px-1">
                        {bot}
                      </span>
                    )}
                    {botRequiredMissing && (
                      <span
                        className="w-1.5 h-1.5 rounded-full bg-warning-fg"
                        title="Required arguments missing"
                      />
                    )}
                  </span>
                ),
              },
            ]}
            panels={{
              ticket: (
                <TicketTab
                  board={board}
                  initial={initial}
                  title={title}
                  setTitle={setTitle}
                  body={body}
                  setBody={setBody}
                  state={state}
                  setState={setState}
                  priority={priority}
                  setPriority={setPriority}
                  labels={labels}
                  setLabels={setLabels}
                  assignee={assignee}
                  setAssignee={setAssignee}
                  fields={fields}
                  setFields={setFields}
                />
              ),
              bot: (
                <BotTab
                  bots={bots}
                  botsError={botsError}
                  bot={bot}
                  setBot={(name) => {
                    setBot(name);
                    if (name === "") setBotArgs({});
                  }}
                  botArgs={botArgs}
                  setBotArgs={setBotArgs}
                  selectedBot={selectedBot}
                />
              ),
            }}
          />
        </div>

        {submitError && (
          <div className="px-4 pb-2 text-xs text-red-400" role="alert">
            {submitError}
          </div>
        )}
        <footer className="px-4 py-2.5 border-t border-border-default flex items-center justify-between bg-surface-0">
          <div>
            {onDelete && (
              <button
                type="button"
                onClick={onDelete}
                disabled={submitting}
                className="text-xs text-red-400 hover:underline disabled:opacity-50 disabled:cursor-not-allowed"
              >
                Delete
              </button>
            )}
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={onClose}
              disabled={submitting}
              className="text-xs px-3 py-1.5 rounded border border-border-default hover:bg-surface-2 disabled:opacity-50 disabled:cursor-not-allowed"
            >
              Cancel
            </button>
            <button
              type="submit"
              disabled={submitting}
              className="text-xs px-3 py-1.5 rounded bg-accent text-on-accent hover:opacity-90 disabled:opacity-60 disabled:cursor-not-allowed"
            >
              {submitting ? "Saving…" : initial ? "Save" : "Create"}
            </button>
          </div>
        </footer>
      </form>
    </Dialog>
  );
}

interface TicketTabProps {
  board: NativeBoard;
  initial: NativeIssue | null;
  title: string;
  setTitle: (v: string) => void;
  body: string;
  setBody: (v: string) => void;
  state: string;
  setState: (v: string) => void;
  priority: number;
  setPriority: (v: number) => void;
  labels: string[];
  setLabels: (v: string[]) => void;
  assignee: string;
  setAssignee: (v: string) => void;
  fields: Record<string, string>;
  setFields: (v: Record<string, string>) => void;
}

function TicketTab({
  board,
  initial,
  title,
  setTitle,
  body,
  setBody,
  state,
  setState,
  priority,
  setPriority,
  labels,
  setLabels,
  assignee,
  setAssignee,
  fields,
  setFields,
}: TicketTabProps) {
  return (
    <div className="space-y-3 py-3">
      <Field label="Title" required>
        <Input
          autoFocus
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          size="md"
          required
        />
      </Field>

      <Field label="Body">
        <MarkdownPreview
          value={body}
          onChange={setBody}
          rows={5}
          placeholder="Add context, repro steps, or notes…"
        />
      </Field>

      <div className="grid grid-cols-2 gap-3">
        <Field label="State">
          <Select
            value={state}
            onChange={(e) => setState(e.target.value)}
            size="md"
            disabled={!!initial /* edits go through transition for the audit log */}
          >
            {board.states.map((s) => (
              <option key={s.name} value={s.name}>
                {s.display ?? s.name}
              </option>
            ))}
          </Select>
          {initial && (
            <p className="text-xs text-fg-muted mt-1">
              Drag the card to move between states.
            </p>
          )}
        </Field>
        <Field label="Priority">
          <Input
            type="number"
            value={priority}
            onChange={(e) => setPriority(parseInt(e.target.value || "0", 10))}
            size="md"
          />
        </Field>
      </div>

      <div className="grid grid-cols-2 gap-3">
        <Field label="Labels">
          <TagInput value={labels} onChange={setLabels} placeholder="urgent, infra…" />
        </Field>
        <Field label="Assignee">
          <Input
            value={assignee}
            onChange={(e) => setAssignee(e.target.value)}
            size="md"
            placeholder="someone@…"
          />
        </Field>
      </div>

      {(board.fields ?? []).map((f) => (
        <Field key={f.name} label={(f.display ?? f.name) + ` (${f.type})`}>
          {f.type === "enum" ? (
            <Select
              value={fields[f.name] ?? ""}
              onChange={(e) =>
                setFields({ ...fields, [f.name]: e.target.value })
              }
              size="md"
            >
              <option value="">(unset)</option>
              {(f.enum_values ?? []).map((v) => (
                <option key={v} value={v}>
                  {v}
                </option>
              ))}
            </Select>
          ) : f.type === "bool" ? (
            <label className="inline-flex items-center gap-2">
              <input
                type="checkbox"
                checked={fields[f.name] === "true"}
                onChange={(e) =>
                  setFields({
                    ...fields,
                    [f.name]: e.target.checked ? "true" : "false",
                  })
                }
                className="accent-accent"
              />
              <span className="text-xs text-fg-muted">
                {fields[f.name] === "true" ? "true" : "false"}
              </span>
            </label>
          ) : (
            <Input
              type={
                f.type === "number"
                  ? "number"
                  : f.type === "date"
                    ? "datetime-local"
                    : "text"
              }
              value={fields[f.name] ?? ""}
              onChange={(e) =>
                setFields({ ...fields, [f.name]: e.target.value })
              }
              size="md"
              required={f.required}
            />
          )}
        </Field>
      ))}
    </div>
  );
}

interface BotTabProps {
  bots: BotEntryWithSchema[] | null;
  botsError: string | null;
  bot: string;
  setBot: (v: string) => void;
  botArgs: Record<string, string>;
  setBotArgs: (next: Record<string, string>) => void;
  selectedBot: BotEntryWithSchema | null;
}

function BotTab({
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
        <p className="text-[11px] text-fg-subtle mt-1">
          When set, this bot overrides the dispatcher's per-assignee or
          global workflow selection for this ticket.
        </p>
      </Field>

      {bot && (
        <Field label="Arguments">
          <BotArgsForm
            bot={selectedBot}
            values={botArgs}
            onChange={setBotArgs}
          />
        </Field>
      )}

      {!bot && Object.keys(botArgs).length > 0 && (
        <Field label="Saved arguments (no bot selected)">
          <BotArgsForm
            bot={{ name: "", path: "", vars: { fields: [] } }}
            values={botArgs}
            onChange={setBotArgs}
          />
        </Field>
      )}
    </div>
  );
}

function Field({
  label,
  children,
  required,
}: {
  label: string;
  required?: boolean;
  children: React.ReactNode;
}) {
  return (
    <label className="block">
      <span className="text-xs text-fg-muted mb-1 flex items-baseline gap-2">
        {label}
        {required && <RequiredPill />}
      </span>
      {children}
    </label>
  );
}

// coerceFields converts the modal's string-keyed state map into the
// typed shape the API expects (numbers, bools, etc.). Date fields are
// expected as RFC3339 strings — the datetime-local input emits
// "YYYY-MM-DDThh:mm" which is acceptable since the server stores it
// verbatim and only validates parseability.
function coerceFields(
  board: NativeBoard,
  raw: Record<string, string>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of board.fields ?? []) {
    const v = raw[f.name];
    if (v == null || v === "") continue;
    switch (f.type) {
      case "number": {
        const n = Number(v);
        if (Number.isFinite(n)) out[f.name] = n;
        break;
      }
      case "bool":
        out[f.name] = v === "true";
        break;
      case "date":
        out[f.name] = v.includes("Z") || v.includes("+") ? v : v + ":00Z";
        break;
      default:
        out[f.name] = v;
    }
  }
  return out;
}
