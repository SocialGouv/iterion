import { useEffect, useMemo, useState } from "react";

import type { BotEntryWithSchema } from "@/api/bots";
import {
  createPipelineTask,
  type CreatePipelineTaskInput,
} from "@/api/pipelineBoards";
import { defaultStringFor } from "@/components/shared/VarFieldInput";
import {
  Button,
  Checkbox,
  Dialog,
  InlineBanner,
  Input,
  TagInput,
  Textarea,
} from "@/components/ui";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { isVarMissing } from "@/lib/varValidation";
import { useBotsStore } from "@/store/bots";
import { BotArgsForm } from "@/views/Board/BotArgsForm";
import { BotPicker } from "@/views/Board/BotPicker";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: () => void;
}

export default function AddTaskDialog(props: Props) {
  // Mount a fresh form for every open cycle. Besides making reset semantics
  // obvious, this avoids synchronously mirroring `open` into state.
  if (!props.open) return null;
  return <AddTaskDialogContent {...props} />;
}

function AddTaskDialogContent({ open, onOpenChange, onCreated }: Props) {
  const [botName, setBotName] = useState("");
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [labels, setLabels] = useState<string[]>([]);
  const [priority, setPriority] = useState(0);
  const [botArgs, setBotArgs] = useState<Record<string, string>>({});
  const [start, setStart] = useState(false);
  const action = useAsyncAction();

  // Shared bot catalog store — fetched once across all consumers (Home,
  // BotPicker, Inspector, Catalog manager). The board is global, so the
  // operator picks which bot runs this task here.
  const bots = useBotsStore((s) => s.bots);
  const botsError = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    if (bots === null) void fetchBots();
  }, [bots, fetchBots]);

  const selectedBot: BotEntryWithSchema | null = useMemo(() => {
    if (!botName || !bots) return null;
    return bots.find((b) => b.name === botName) ?? null;
  }, [botName, bots]);

  const botEnabled = selectedBot?.enabled !== false;

  const missingRequiredArgs = useMemo(() => {
    if (!selectedBot?.vars?.fields) return false;
    return selectedBot.vars.fields.some((field) =>
      isVarMissing(field, botArgs[field.name] ?? defaultStringFor(field)),
    );
  }, [selectedBot, botArgs]);

  const canSubmit =
    title.trim().length > 0 && botName.trim().length > 0 && !missingRequiredArgs;

  const submit = async () => {
    if (!canSubmit) {
      action.setError(
        botName.trim().length === 0
          ? "Choose a bot for this task."
          : missingRequiredArgs
            ? "Required bot arguments are missing."
            : "A task title is required.",
      );
      return;
    }
    const input: CreatePipelineTaskInput = {
      bot: botName.trim(),
      title: title.trim(),
      ...(body.trim() ? { body: body.trim() } : {}),
      ...(labels.length > 0 ? { labels } : {}),
      ...(priority !== 0 ? { priority } : {}),
      ...(Object.keys(botArgs).length > 0 ? { bot_args: botArgs } : {}),
      ...(start && botEnabled ? { start: true } : {}),
    };
    const result = await action.run(() => createPipelineTask(input));
    if (result === undefined) return;
    onOpenChange(false);
    onCreated();
  };

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="Add pipeline task"
      description="Create a task for the pipeline board. Pick the bot that will run it."
      widthClass="max-w-2xl"
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            variant="primary"
            loading={action.busy}
            disabled={!canSubmit}
            onClick={() => void submit()}
          >
            {start && botEnabled ? "Create & start" : "Add to Todo"}
          </Button>
        </>
      }
    >
      <div className="max-h-[68vh] space-y-4 overflow-y-auto pr-1">
        {action.error && (
          <InlineBanner tone="danger" layout="inline">
            {action.error}
          </InlineBanner>
        )}

        <div>
          <div className="mb-1 text-xs text-fg-muted">Bot</div>
          {botsError ? (
            <div className="text-xs text-warning-fg">Could not load bots: {botsError}</div>
          ) : bots == null ? (
            <div className="text-xs italic text-fg-subtle">Loading bots…</div>
          ) : bots.length === 0 ? (
            <div className="text-xs italic text-fg-subtle">
              No bots discovered. Configure <code>--bots-path</code> on the studio.
            </div>
          ) : (
            <BotPicker value={botName} bots={bots} onChange={setBotName} />
          )}
        </div>

        <label className="block">
          <span className="mb-1 block text-xs text-fg-muted">Title</span>
          <Input
            value={title}
            onChange={(event) => setTitle(event.target.value)}
            placeholder="What should this pipeline do?"
            size="md"
            autoFocus
            required
          />
        </label>

        <label className="block">
          <span className="mb-1 block text-xs text-fg-muted">Description</span>
          <Textarea
            value={body}
            onChange={(event) => setBody(event.target.value)}
            placeholder="Context, acceptance criteria, links…"
            rows={4}
          />
        </label>

        <div>
          <div className="mb-1 text-xs text-fg-muted">Labels</div>
          <TagInput value={labels} onChange={setLabels} placeholder="Add label…" />
        </div>

        <label className="block max-w-40">
          <span className="mb-1 block text-xs text-fg-muted">Priority</span>
          <Input
            type="number"
            value={String(priority)}
            onChange={(event) => setPriority(Number(event.target.value) || 0)}
            min={0}
          />
        </label>

        <div className="border-t border-border-default pt-4">
          <div className="mb-3 text-caption uppercase tracking-wide text-fg-subtle">
            Bot arguments
          </div>
          <BotArgsForm
            bot={botName ? selectedBot : null}
            values={botArgs}
            onChange={setBotArgs}
          />
          {missingRequiredArgs && (
            <p className="mt-2 text-xs text-warning-fg">
              Fill every required bot argument before creating the task.
            </p>
          )}
        </div>

        <div className="border-t border-border-default pt-4">
          <Checkbox
            checked={start}
            onChange={(event) => setStart(event.target.checked)}
            disabled={!botName || !botEnabled}
            label="Start immediately"
            help={
              !botName
                ? "Pick a bot first."
                : botEnabled
                  ? "Otherwise the task stays in Todo until an operator or integration starts it."
                  : "This bot is disabled. You can still add the task to Todo, but it cannot start yet."
            }
          />
        </div>
      </div>
    </Dialog>
  );
}
