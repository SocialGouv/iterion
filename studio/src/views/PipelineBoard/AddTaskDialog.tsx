import { useMemo, useState } from "react";

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
import { BotArgsForm } from "@/views/Board/BotArgsForm";

interface Props {
  open: boolean;
  botID: string;
  bot: BotEntryWithSchema | null;
  botEnabled: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: () => void;
}

export default function AddTaskDialog(props: Props) {
  // Mount a fresh form for every open cycle. Besides making reset semantics
  // obvious, this avoids synchronously mirroring `open` into six state values
  // from an effect.
  if (!props.open) return null;
  return <AddTaskDialogContent {...props} />;
}

function AddTaskDialogContent({
  open,
  botID,
  bot,
  botEnabled,
  onOpenChange,
  onCreated,
}: Props) {
  const [title, setTitle] = useState("");
  const [body, setBody] = useState("");
  const [labels, setLabels] = useState<string[]>([]);
  const [priority, setPriority] = useState(0);
  const [botArgs, setBotArgs] = useState<Record<string, string>>({});
  const [start, setStart] = useState(false);
  const action = useAsyncAction();

  const missingRequiredArgs = useMemo(() => {
    if (!bot?.vars?.fields) return false;
    return bot.vars.fields.some((field) =>
      isVarMissing(field, botArgs[field.name] ?? defaultStringFor(field)),
    );
  }, [bot, botArgs]);

  const canSubmit = title.trim().length > 0 && !missingRequiredArgs;

  const submit = async () => {
    if (!canSubmit) {
      action.setError(
        missingRequiredArgs
          ? "Required bot arguments are missing."
          : "A task title is required.",
      );
      return;
    }
    const input: CreatePipelineTaskInput = {
      title: title.trim(),
      ...(body.trim() ? { body: body.trim() } : {}),
      ...(labels.length > 0 ? { labels } : {}),
      ...(priority !== 0 ? { priority } : {}),
      ...(Object.keys(botArgs).length > 0 ? { bot_args: botArgs } : {}),
      start,
    };
    const result = await action.run(() => createPipelineTask(botID, input));
    if (result === undefined) return;
    onOpenChange(false);
    onCreated();
  };

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="Add pipeline task"
      description={`Create a task for ${bot?.display_name?.trim() || botID}. The bot is fixed by this board.`}
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
            {start ? "Create & start" : "Add to Todo"}
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

        <div className="rounded-md border border-border-default bg-surface-2 px-3 py-2">
          <div className="text-caption uppercase tracking-wide text-fg-subtle">Bot</div>
          <div className="mt-0.5 flex items-center gap-2 text-sm font-medium text-fg-default">
            {bot?.icon && <span aria-hidden>{bot.icon}</span>}
            <span>{bot?.display_name?.trim() || botID}</span>
            {bot?.display_name?.trim() && (
              <code className="text-micro font-normal text-fg-subtle">{botID}</code>
            )}
          </div>
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
          <BotArgsForm bot={bot} values={botArgs} onChange={setBotArgs} />
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
            disabled={!botEnabled}
            label="Start immediately"
            help={
              botEnabled
                ? "Otherwise the task stays in Todo until an operator or integration starts it."
                : "This bot is disabled. You can still add the task to Todo, but it cannot start yet."
            }
          />
        </div>
      </div>
    </Dialog>
  );
}
