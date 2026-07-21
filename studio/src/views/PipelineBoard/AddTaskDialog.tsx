import { useEffect, useMemo, useState } from "react";
import { ChevronDownIcon, ChevronRightIcon } from "@radix-ui/react-icons";

import type { BotEntryWithSchema } from "@/api/bots";
import { forgeTeamRepoKey } from "@/api/forgeConnections";
import type { VarField } from "@/api/types";
import {
  createPipelineTask,
  updatePipelineTask,
  type CreatePipelineTaskInput,
  type PipelineBoardCard,
  type PipelineTaskPatch,
} from "@/api/pipelineBoards";
import VarFieldInput, {
  defaultStringFor,
} from "@/components/shared/VarFieldInput";
import {
  Button,
  Checkbox,
  Dialog,
  InlineBanner,
  Input,
  TagInput,
  Textarea,
} from "@/components/ui";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { isVarMissing, isVarRequired, RequiredPill } from "@/lib/varValidation";
import { useBotsStore } from "@/store/bots";
import { BotPicker } from "@/views/Board/BotPicker";
import { RepositoryField } from "@/views/Board/issueModal/RepositoryField";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: () => void;
  // When set, the dialog edits this existing (not-yet-run Todo, or failed
  // Closed) ticket instead of creating a new one: the form pre-fills from the
  // card and Save PATCHes it. The ready-state stays under the card's Mark
  // ready / Unmark buttons, so the "Ready to run" checkbox is hidden in edit
  // mode.
  editTask?: PipelineBoardCard;
}

// coerceEntryInput turns a card's entry_input (launch vars / bot_args, whose
// values may be non-strings) into the string map the arg form edits.
// Objects/arrays pretty-print so they are editable in the json Textarea.
function coerceEntryInput(
  input: Record<string, unknown> | undefined,
): Record<string, string> {
  const out: Record<string, string> = {};
  if (!input) return out;
  for (const [key, value] of Object.entries(input)) {
    if (value === null || value === undefined) continue;
    out[key] =
      typeof value === "string"
        ? value
        : typeof value === "number" || typeof value === "boolean"
          ? String(value)
          : JSON.stringify(value, null, 2);
  }
  return out;
}

/**
 * isPrimaryVar decides which of a bot's vars is the pipeline's *primary
 * input* — the one thing the operator actually chooses — versus a
 * technical parameter that belongs behind the "Advanced" accordion.
 *
 * A var is primary when the bot author left it for the operator to fill:
 * a non-bool var whose resolved default is empty (no default at all, or a
 * placeholder like `requested_character = ""`). Bools and vars carrying a
 * concrete default (`max_parallel = 5`, `type_id = "…"`) are technical.
 * This keeps the form honest without any per-bot config: e.g. the
 * historical-series `main` surfaces only its character, and `episode`
 * surfaces character + episode_index.
 */
function isPrimaryVar(field: VarField): boolean {
  if (field.type === "bool") return false;
  return defaultStringFor(field).trim() === "";
}

export default function AddTaskDialog(props: Props) {
  // Mount a fresh form for every open cycle. Besides making reset semantics
  // obvious, this avoids synchronously mirroring `open` into state.
  if (!props.open) return null;
  return <AddTaskDialogContent {...props} />;
}

function VarFieldRow({
  field,
  value,
  onChange,
}: {
  field: VarField;
  value: string;
  onChange: (v: string) => void;
}) {
  const required = isVarRequired(field);
  const invalid = isVarMissing(field, value);
  return (
    <div className="grid grid-cols-[160px_1fr] items-start gap-3">
      <label htmlFor={`bot-arg-${field.name}`} className="pt-1">
        <div className="flex items-baseline gap-2">
          <span className="font-mono text-xs font-medium">{field.name}</span>
          {required && <RequiredPill />}
        </div>
        <div className="text-caption text-fg-subtle">{field.type}</div>
      </label>
      <VarFieldInput
        field={field}
        value={value}
        onChange={onChange}
        required={required}
        invalid={invalid}
      />
    </div>
  );
}

function AddTaskDialogContent({
  open,
  onOpenChange,
  onCreated,
  editTask,
}: Props) {
  const isEdit = !!editTask;
  const [botName, setBotName] = useState(editTask?.bot_id ?? "");
  const [title, setTitle] = useState(editTask?.title ?? "");
  const [body, setBody] = useState(editTask?.body ?? "");
  const [labels, setLabels] = useState<string[]>(editTask?.labels ?? []);
  const [priority, setPriority] = useState(editTask?.priority ?? 0);
  const [botArgs, setBotArgs] = useState<Record<string, string>>(() =>
    coerceEntryInput(editTask?.entry_input),
  );
  // Hard deps: native issue IDs that must reach done before this ticket launches.
  const [blockersText, setBlockersText] = useState(() =>
    (editTask?.blockers ?? []).map((b) => b.id).join("\n"),
  );
  const [start, setStart] = useState(false);
  // Open Advanced by default when editing so the title / description / labels /
  // priority the operator came to change are visible without a click.
  const [advancedOpen, setAdvancedOpen] = useState(isEdit);
  const action = useAsyncAction();

  // Repo-first scoping: the picker is fed by the same connected-repos list
  // the /board IssueModal uses (cloud-only, gated on `enabled`). Pre-fill on
  // create from the sidebar's active repo (overview mode = no default); on
  // edit, keep whatever the card already has (matched on connection_id +
  // repo when both are set, else on repo alone — the run-only fallback).
  const {
    activeRepo,
    overview,
    repos: connectedRepos,
    enabled: repoScopeEnabled,
  } = useActiveRepo();
  const initialRepoKey = useMemo(() => {
    const ex = editTask?.external;
    if (ex?.connection_id && ex.repo) {
      return `${ex.connection_id}::${ex.repo}`;
    }
    if (ex?.repo) {
      // Run-only fallback (project_path): match by repo suffix against the
      // connected list so the picker shows the operator's actual repo when
      // it lines up, otherwise leaves the field empty ("No repository").
      const hit = connectedRepos.find(
        (r) => ex.repo === r.repo_full_name || ex.repo.endsWith("/" + r.repo_full_name),
      );
      if (hit) return forgeTeamRepoKey(hit);
      return "";
    }
    if (!isEdit && repoScopeEnabled && !overview && activeRepo) {
      return forgeTeamRepoKey(activeRepo);
    }
    return "";
  }, [editTask, isEdit, repoScopeEnabled, overview, activeRepo, connectedRepos]);
  const [repoKey, setRepoKey] = useState(initialRepoKey);
  // Re-seed on identity change (edit target swap, active-repo hydration).
  useEffect(() => {
    setRepoKey(initialRepoKey);
  }, [initialRepoKey]);

  // Shared bot catalog store — fetched once across all consumers. The board
  // is global, so the operator picks which bot runs this task here.
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
  const hasSchemaError = Boolean(selectedBot?.schema_error);

  const fields: VarField[] = useMemo(
    () => (selectedBot?.vars?.fields ?? []) as VarField[],
    [selectedBot],
  );
  const primaryFields = useMemo(() => fields.filter(isPrimaryVar), [fields]);
  const technicalFields = useMemo(
    () => fields.filter((f) => !isPrimaryVar(f)),
    [fields],
  );

  const argValue = (f: VarField) => botArgs[f.name] ?? defaultStringFor(f);
  const setArg = (name: string, v: string) =>
    setBotArgs((prev) => ({ ...prev, [name]: v }));

  const missingRequiredArgs = useMemo(
    () =>
      fields.some((f) =>
        isVarMissing(f, botArgs[f.name] ?? defaultStringFor(f)),
      ),
    [fields, botArgs],
  );

  // The repo picker is only offered in cloud mode with at least one connected
  // repo (or a pre-existing forge-linked external so we don't hide the
  // linkage on an edit). Outside cloud mode: no repo affordance.
  const hasRepoAffordance =
    repoScopeEnabled &&
    (connectedRepos.length > 0 || !!editTask?.external);
  // Resolves the picker selection back to the external payload the server
  // accepts: connected repo wins (full provider/connection_id/repo_full_name);
  // else surface the pre-existing edit link untouched (the run-only fallback,
  // or a repo whose connection was since removed).
  const resolveExternalForSubmit = () => {
    if (!hasRepoAffordance) return undefined;
    if (!repoKey) return undefined;
    const picked = connectedRepos.find((r) => forgeTeamRepoKey(r) === repoKey);
    if (picked) {
      return {
        provider: picked.provider,
        connection_id: picked.connection_id,
        repo: picked.repo_full_name,
      };
    }
    if (
      editTask?.external &&
      repoKey === `${editTask.external.connection_id}::${editTask.external.repo}`
    ) {
      return {
        provider: editTask.external.provider,
        connection_id: editTask.external.connection_id,
        repo: editTask.external.repo,
      };
    }
    return undefined;
  };

  // The title auto-derives from the primary inputs so the operator only has
  // to pick the input principal; they can still override it under Advanced.
  const derivedTitle = useMemo(() => {
    const values = primaryFields
      .map((f) => (botArgs[f.name] ?? "").trim())
      .filter(Boolean);
    if (values.length > 0) return values.join(" · ");
    return selectedBot?.display_name?.trim() || botName.trim();
  }, [primaryFields, botArgs, selectedBot, botName]);

  const effectiveTitle = title.trim() || derivedTitle;
  const canSubmit =
    botName.trim().length > 0 &&
    effectiveTitle.length > 0 &&
    !missingRequiredArgs;

  const submit = async () => {
    if (!canSubmit) {
      action.setError(
        botName.trim().length === 0
          ? "Choose a bot for this task."
          : missingRequiredArgs
            ? "Fill the required inputs before saving the task."
            : "A task title is required.",
      );
      return;
    }
    const external = resolveExternalForSubmit();
    const blockers = blockersText
      .split(/[\n,]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    if (isEdit && editTask?.issue_id) {
      const patch: PipelineTaskPatch = {
        bot: botName.trim(),
        title: effectiveTitle,
        body: body.trim(),
        labels,
        priority,
        bot_args: botArgs,
        ...(external ? { external } : {}),
        blockers,
      };
      const result = await action.run(() =>
        updatePipelineTask(editTask.issue_id as string, patch),
      );
      if (result === undefined) return;
      onOpenChange(false);
      onCreated();
      return;
    }
    const input: CreatePipelineTaskInput = {
      bot: botName.trim(),
      title: effectiveTitle,
      ...(body.trim() ? { body: body.trim() } : {}),
      ...(labels.length > 0 ? { labels } : {}),
      ...(priority !== 0 ? { priority } : {}),
      ...(Object.keys(botArgs).length > 0 ? { bot_args: botArgs } : {}),
      ...(blockers.length > 0 ? { blockers } : {}),
      ...(start && botEnabled ? { start: true } : {}),
      ...(external ? { external } : {}),
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
      title={isEdit ? "Edit ticket" : "Add pipeline task"}
      description={
        isEdit
          ? "Edit this Opened ticket before it runs. Technical parameters sit under Advanced."
          : "Pick the pipeline to run and its input. Technical parameters sit under Advanced."
      }
      widthClass="max-w-2xl"
      footer={
        <>
          {/* The launch-intent toggle lives in the footer, next to the
              button whose label it flips — never below the scroll fold. */}
          {!isEdit && (
            <div className="mr-auto">
              <Checkbox
                checked={start}
                onChange={(event) => setStart(event.target.checked)}
                disabled={!botName || !botEnabled}
                label="Ready to run"
                help={
                  !botName
                    ? "Pick a pipeline first."
                    : botEnabled
                      ? "Starts automatically when a concurrency slot frees. Otherwise the ticket waits in Backlog until you stage it with its “→ Todo” button."
                      : "This bot is disabled. The ticket stays in Backlog; enable the bot, then stage it with “→ Todo” to run."
                }
              />
            </div>
          )}
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            variant="primary"
            loading={action.busy}
            disabled={!canSubmit}
            onClick={() => void submit()}
          >
            {isEdit ? "Save" : start && botEnabled ? "Add ready to Opened" : "Add to Opened"}
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
          <div className="mb-1 text-xs text-fg-muted">Pipeline</div>
          {botsError ? (
            <div className="text-xs text-warning-fg">
              Could not load bots: {botsError}
            </div>
          ) : bots == null ? (
            <div className="text-xs italic text-fg-subtle">Loading bots…</div>
          ) : bots.length === 0 ? (
            <div className="text-xs italic text-fg-subtle">
              No bots discovered. Configure <code>--bots-path</code> on the
              studio.
            </div>
          ) : (
            <BotPicker value={botName} bots={bots} onChange={setBotName} />
          )}
        </div>

        {hasRepoAffordance && (
          <RepositoryField
            repos={connectedRepos}
            value={repoKey}
            onChange={setRepoKey}
            legacyLinkedLabel={editTask?.external?.repo ?? null}
          />
        )}

        {/* Primary input(s): the one thing the operator actually chooses. */}
        {selectedBot && !hasSchemaError && primaryFields.length > 0 && (
          <div className="space-y-3">
            {primaryFields.map((f) => (
              <VarFieldRow
                key={f.name}
                field={f}
                value={argValue(f)}
                onChange={(v) => setArg(f.name, v)}
              />
            ))}
          </div>
        )}
        {selectedBot &&
          !hasSchemaError &&
          fields.length > 0 &&
          primaryFields.length === 0 && (
            <p className="text-micro italic text-fg-subtle">
              This pipeline takes no primary input — every parameter has a
              default and lives under Advanced.
            </p>
          )}
        {hasSchemaError && (
          <div className="text-micro text-warning-fg">
            The bot&apos;s workflow failed to parse, so its inputs can&apos;t be
            shown here. You can still add the task; only keys the workflow
            declares as vars take effect.
          </div>
        )}

        {/* Advanced: title override, technical params, tracker metadata. */}
        <div className="border-t border-border-default pt-3">
          <button
            type="button"
            onClick={() => setAdvancedOpen((o) => !o)}
            aria-expanded={advancedOpen}
            className="flex w-full items-center gap-1.5 text-caption uppercase tracking-wide text-fg-subtle transition-colors hover:text-fg-default"
          >
            {advancedOpen ? (
              <ChevronDownIcon className="h-3.5 w-3.5" />
            ) : (
              <ChevronRightIcon className="h-3.5 w-3.5" />
            )}
            Advanced parameters
          </button>

          {advancedOpen && (
            <div className="mt-3 space-y-4">
              <label className="block">
                <span className="mb-1 block text-xs text-fg-muted">Title</span>
                <Input
                  value={title}
                  onChange={(event) => setTitle(event.target.value)}
                  placeholder={derivedTitle || "What should this pipeline do?"}
                  size="md"
                />
                <span className="mt-1 block text-micro text-fg-subtle">
                  Leave blank to name the card{" "}
                  <span className="font-mono">{derivedTitle || "…"}</span>.
                </span>
              </label>

              {selectedBot && !hasSchemaError && technicalFields.length > 0 && (
                <div className="space-y-3">
                  {technicalFields.map((f) => (
                    <VarFieldRow
                      key={f.name}
                      field={f}
                      value={argValue(f)}
                      onChange={(v) => setArg(f.name, v)}
                    />
                  ))}
                </div>
              )}

              <label className="block">
                <span className="mb-1 block text-xs text-fg-muted">
                  Description
                </span>
                <Textarea
                  value={body}
                  onChange={(event) => setBody(event.target.value)}
                  placeholder="Context, acceptance criteria, links…"
                  rows={3}
                />
              </label>

              <div>
                <div className="mb-1 text-xs text-fg-muted">Labels</div>
                <TagInput
                  value={labels}
                  onChange={setLabels}
                  placeholder="Add label…"
                />
              </div>

              <label className="block max-w-40">
                <span className="mb-1 block text-xs text-fg-muted">
                  Priority
                </span>
                <Input
                  type="number"
                  value={String(priority)}
                  onChange={(event) =>
                    setPriority(Number(event.target.value) || 0)
                  }
                  min={0}
                  title="Higher numbers launch first once ready; equal priorities go oldest-first. 0 = unprioritized."
                />
                <span className="mt-1 block text-micro text-fg-subtle">
                  Higher launches first once ready.
                </span>
              </label>

              <label className="block">
                <span className="mb-1 block text-xs text-fg-muted">
                  Blockers (hard deps)
                </span>
                <Textarea
                  value={blockersText}
                  onChange={(event) => setBlockersText(event.target.value)}
                  placeholder={"native:…\nnative:…"}
                  rows={3}
                  title="Issue IDs that must reach Done before this ticket can launch. One per line or comma-separated."
                />
                <span className="mt-1 block text-micro text-fg-subtle">
                  Ticket IDs that must finish (state <code>done</code>) before
                  this one can launch. Cycles are rejected. Open blockers park
                  the ticket in Waiting on deps instead of Ready.
                </span>
              </label>
            </div>
          )}
        </div>


        {!isEdit && (
          <div className="border-t border-border-default pt-4">
            <Checkbox
              checked={start}
              onChange={(event) => setStart(event.target.checked)}
              disabled={!botName || !botEnabled}
              label="Ready to run"
              help={
                !botName
                  ? "Pick a pipeline first."
                  : botEnabled
                    ? "Marks the ticket ready — it starts automatically when a concurrency slot frees. Otherwise it stays in Opened as a draft until you Mark ready."
                    : "This bot is disabled. The ticket stays in Opened as a draft; enable the bot, then Mark ready to run."
              }
            />
          </div>
        )}
      </div>
    </Dialog>
  );
}
