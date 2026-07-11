import { useEffect, useMemo, useState } from "react";

import { type BotEntryWithSchema } from "@/api/bots";
import {
  type NativeBoard,
  type NativeIssue,
} from "@/api/native";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Tabs } from "@/components/ui/Tabs";
import { defaultStringFor } from "@/components/shared/VarFieldInput";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { isVarMissing } from "@/lib/varValidation";
import { useBotsStore } from "@/store/bots";

import { BotTab } from "./issueModal/BotTab";
import { TicketTab } from "./issueModal/TicketTab";

interface Props {
  board: NativeBoard;
  initial: NativeIssue | null;
  onSubmit: (input: Partial<NativeIssue>) => Promise<void> | void;
  onClose: () => void;
  onDelete?: () => void;
  // When set, the issue is in a pre-dispatch lane (inbox/backlog) and a
  // "Let's go" button is shown that transitions it into the dispatch
  // lane so the running dispatcher picks it up. Omitted otherwise.
  onDispatch?: () => void;
  // Existing assignees across the board, seeding the assignee autocomplete.
  allAssignees: string[];
}

export default function IssueModal({ board, initial, onSubmit, onClose, onDelete, onDispatch, allAssignees }: Props) {
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
  const submitAction = useAsyncAction();
  const [fields, setFields] = useState<Record<string, string>>(() => {
    const out: Record<string, string> = {};
    for (const f of board.fields ?? []) {
      const v = initial?.fields?.[f.name];
      out[f.name] = v == null ? "" : String(v);
    }
    return out;
  });

  // Bots catalog. Shared zustand store — fetched once across all consumers
  // (Home, BotPicker, Inspector, Catalog manager). Loading + error
  // surface separately so the Bot tab degrades gracefully.
  const bots = useBotsStore((s) => s.bots);
  const botsError = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    if (bots === null) void fetchBots();
  }, [bots, fetchBots]);

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
    if (submitAction.busy) return;
    if (botRequiredMissing) {
      setTab("bot");
      submitAction.setError("Required bot arguments are missing.");
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
    await submitAction.run(() => Promise.resolve(onSubmit(out)));
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
                      <Badge variant="accent" size="sm" className="font-mono">
                        {bot}
                      </Badge>
                    )}
                    {botRequiredMissing && (
                      <>
                        <span
                          role="img"
                          aria-label="Required arguments missing"
                          className="w-1.5 h-1.5 rounded-full bg-warning-fg"
                          title="Required arguments missing"
                        />
                        <span className="sr-only">Required arguments missing</span>
                      </>
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
                  allAssignees={allAssignees}
                  fields={fields}
                  setFields={setFields}
                />
              ),
              bot: (
                <BotTab
                  bots={bots}
                  botsError={botsError}
                  bot={bot}
                  setBot={setBot}
                  botArgs={botArgs}
                  setBotArgs={setBotArgs}
                  selectedBot={selectedBot}
                />
              ),
            }}
          />
        </div>

        {submitAction.error && (
          <div className="px-4 pb-2">
            <InlineBanner tone="danger" layout="inline">
              {submitAction.error}
            </InlineBanner>
          </div>
        )}
        <footer className="px-4 py-2.5 border-t border-border-default flex items-center justify-between bg-surface-0">
          <div className="flex items-center gap-3">
            {onDispatch && (
              <Button
                type="button"
                variant="success"
                size="sm"
                onClick={onDispatch}
                disabled={submitAction.busy}
              >
                ▶ Let's go
              </Button>
            )}
            {onDispatch && onDelete && (
              <span className="h-4 w-px bg-border-default" aria-hidden="true" />
            )}
            {onDelete && (
              <Button
                type="button"
                variant="danger"
                size="sm"
                onClick={onDelete}
                disabled={submitAction.busy}
              >
                Delete
              </Button>
            )}
          </div>
          <div className="flex items-center gap-2">
            <Button
              type="button"
              variant="secondary"
              size="sm"
              onClick={onClose}
              disabled={submitAction.busy}
            >
              Cancel
            </Button>
            <Button type="submit" variant="primary" size="sm" loading={submitAction.busy}>
              {initial ? "Save" : "Create"}
            </Button>
          </div>
        </footer>
      </form>
    </Dialog>
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
