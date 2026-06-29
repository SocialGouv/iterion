import { useEffect, useMemo, useState } from "react";
import { Link } from "wouter";

import { type BotEntryWithSchema } from "@/api/bots";
import {
  createIssuePull,
  getIssuePullCI,
  listIssuePulls,
  mergeIssuePull,
  type CIRun,
  type MergeMethod,
  type NativeBoard,
  type NativeIssue,
  type PullRef,
} from "@/api/native";
import BranchDiffModal from "@/components/Runs/BranchDiffModal";
import { Badge, type BadgeVariant } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Combobox } from "@/components/ui/Combobox";
import { CopyButton } from "@/components/ui/CopyButton";
import { Dialog } from "@/components/ui/Dialog";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { MarkdownPreview } from "@/components/ui/MarkdownPreview";
import { Select } from "@/components/ui/Select";
import { Tabs } from "@/components/ui/Tabs";
import { TagInput } from "@/components/ui/TagInput";
import VarFieldInput, { defaultStringFor } from "@/components/shared/VarFieldInput";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm, type ConfirmOptions } from "@/hooks/useConfirm";
import { isVarMissing, RequiredPill } from "@/lib/varValidation";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import { BotArgsForm } from "./BotArgsForm";
import { BotPicker } from "./BotPicker";

void VarFieldInput; // re-exported through BotArgsForm; keep import path stable

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
                      <span className="text-caption font-mono bg-accent/15 text-accent-text rounded px-1">
                        {bot}
                      </span>
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
            {onDelete && (
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={onDelete}
                disabled={submitAction.busy}
                className="text-danger hover:text-danger"
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
  allAssignees: string[];
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
  allAssignees,
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
          <Combobox
            value={assignee}
            options={allAssignees.map((a) => ({ value: a, label: `@${a}` }))}
            onChange={(v) => setAssignee(v)}
            placeholder="Search or type a name…"
            size="md"
            freeSolo
          />
        </Field>
      </div>

      {initial && (initial.last_run_id || initial.last_workdir) && (
        <LastRunSection
          runID={initial.last_run_id}
          workdir={initial.last_workdir}
        />
      )}

      {initial && <PullRequestsSection issue={initial} />}

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
              <Checkbox
                checked={fields[f.name] === "true"}
                onChange={(e) =>
                  setFields({
                    ...fields,
                    [f.name]: e.target.checked ? "true" : "false",
                  })
                }
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

// LastRunSection renders a compact "Last run" panel inside the
// Ticket tab when the dispatcher has stamped a run on the issue.
// Surfaces:
//   - A wouter Link to the run console at /runs/<id>.
//   - The worktree path with copy-to-clipboard and vscode:// links
//     so the operator can pivot from the kanban card into a diff
//     inspector without leaving the studio.
//
// Renders nothing when neither runID nor workdir is set; callers
// gate the mount on that condition too.
function LastRunSection({
  runID,
  workdir,
}: {
  runID?: string;
  workdir?: string;
}) {
  const [diffOpen, setDiffOpen] = useState(false);
  if (!runID && !workdir) return null;
  const runLabel = runID ? runID.slice(0, 12) : "";
  return (
    <div className="rounded border border-border-default bg-surface-1 p-2 space-y-1.5">
      <div className="text-micro uppercase tracking-wide text-fg-subtle">
        Last run
      </div>
      {runID && (
        <div className="flex items-center gap-1.5 text-xs">
          <span className="text-fg-muted">Run:</span>
          <Link
            href={`/runs/${encodeURIComponent(runID)}`}
            className="font-mono text-accent-text hover:underline"
            title={`Open run ${runID}`}
          >
            {runLabel}
          </Link>
          <CopyButton value={runID} variant="icon" label="Copy run id" />
          <Button
            variant="secondary"
            size="sm"
            onClick={() => setDiffOpen(true)}
            className="ml-auto"
            title="View this run's full branch diff without leaving the board"
          >
            View diff
          </Button>
        </div>
      )}
      {runID && (
        <BranchDiffModal
          runId={runID}
          open={diffOpen}
          onClose={() => setDiffOpen(false)}
        />
      )}
      {workdir && (
        <div className="flex items-center gap-1.5 text-xs">
          <span className="text-fg-muted">Worktree:</span>
          <code
            className="flex-1 min-w-0 truncate bg-surface-2 px-1 py-0.5 rounded text-micro"
            title={workdir}
          >
            {workdir}
          </code>
          <CopyButton value={workdir} variant="icon" label="Copy worktree path" />
          <a
            href={`vscode://file/${workdir}`}
            className="text-micro px-1.5 py-0.5 rounded border border-border-default hover:bg-surface-2 text-fg-default"
            title="Open the worktree in VS Code (vscode:// URL handler)"
          >
            VS Code
          </a>
        </div>
      )}
    </div>
  );
}

// PullRequestsSection lists the forge pull/merge requests linked to a card,
// each with a compact CI status indicator and an expandable run list. Read-
// only. Cloud-mode only, and rendered only when the card is forge-linked
// (external.repo present) — so a plain native card shows nothing.
function PullRequestsSection({ issue }: { issue: NativeIssue }) {
  const mode = useServerInfoStore((s) => s.info?.mode);
  const addToast = useUIStore((s) => s.addToast);
  const [pulls, setPulls] = useState<PullRef[] | null>(null);
  const [creating, setCreating] = useState(false);
  const loadAction = useAsyncAction();
  const { confirm, dialog } = useConfirm();

  const forgeLinked = !!issue.external?.repo;
  const eligible = mode === "cloud" && forgeLinked;

  const refresh = async () => {
    await loadAction.run(async () => {
      setPulls(await listIssuePulls(issue.id));
    });
  };

  useEffect(() => {
    if (!eligible) {
      setPulls(null);
      return;
    }
    void loadAction.run(async () => {
      setPulls(await listIssuePulls(issue.id));
    });
    // Re-fetch only when the target issue changes; loadAction is stable.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [issue.id, eligible]);

  // Hide entirely for non-cloud / unlinked cards. A forge-linked card with no
  // PRs still renders so the operator can open one.
  if (!eligible) return null;

  const pullList = pulls ?? [];

  return (
    <div className="rounded border border-border-default bg-surface-1 p-2 space-y-2">
      {dialog}
      <div className="flex items-center justify-between gap-2">
        <div className="text-micro uppercase tracking-wide text-fg-subtle">Pull requests</div>
        <button
          type="button"
          onClick={() => setCreating((v) => !v)}
          className="text-micro text-accent-text hover:underline"
        >
          {creating ? "Cancel" : "+ Open PR"}
        </button>
      </div>
      {loadAction.busy && (
        <p className="text-xs text-fg-subtle italic">Loading pull requests…</p>
      )}
      {loadAction.error && (
        <InlineBanner tone="danger" layout="inline">
          {loadAction.error}
        </InlineBanner>
      )}
      {creating && (
        <CreatePullForm
          issueID={issue.id}
          onCreated={async (pr) => {
            setCreating(false);
            addToast(`Opened PR #${pr.number}`, "success");
            await refresh();
          }}
        />
      )}
      {!loadAction.busy && pullList.length === 0 && !loadAction.error && !creating && (
        <p className="text-xs text-fg-subtle">No pull requests linked to this card yet.</p>
      )}
      {pullList.map((pr) => (
        <PullRow
          key={pr.number}
          issueID={issue.id}
          pr={pr}
          confirm={confirm}
          onMerged={async (merged) => {
            addToast(`Merged PR #${merged.number}`, "success");
            await refresh();
          }}
        />
      ))}
    </div>
  );
}

// CreatePullForm is a minimal source→target branch form that opens a PR for a
// forge-linked card (it reuses the card's connection + repo server-side).
function CreatePullForm({
  issueID,
  onCreated,
}: {
  issueID: string;
  onCreated: (pr: PullRef) => void | Promise<void>;
}) {
  const [source, setSource] = useState("");
  const [target, setTarget] = useState("");
  const [draft, setDraft] = useState(false);
  const action = useAsyncAction();

  const submit = async () => {
    if (!source.trim() || !target.trim()) {
      action.setError("Source and target branch are required.");
      return;
    }
    const pr = await action.run(() =>
      createIssuePull(issueID, {
        source_branch: source.trim(),
        target_branch: target.trim(),
        draft,
      }),
    );
    if (pr) await onCreated(pr);
  };

  return (
    <div className="rounded border border-border-subtle bg-surface-2 p-2 space-y-2">
      <div className="grid grid-cols-[auto_1fr] items-center gap-2">
        <label className="text-micro text-fg-subtle">Source</label>
        <Input
          size="sm"
          value={source}
          onChange={(e) => setSource(e.target.value)}
          placeholder="feature/my-branch"
        />
        <label className="text-micro text-fg-subtle">Target</label>
        <Input
          size="sm"
          value={target}
          onChange={(e) => setTarget(e.target.value)}
          placeholder="main"
        />
      </div>
      <label className="inline-flex items-center gap-2">
        <Checkbox checked={draft} onChange={(e) => setDraft(e.target.checked)} />
        <span className="text-micro text-fg-muted">Open as draft</span>
      </label>
      {action.error && (
        <InlineBanner tone="danger" layout="inline">
          {action.error}
        </InlineBanner>
      )}
      <Button
        size="sm"
        onClick={() => void submit()}
        loading={action.busy}
        disabled={action.busy}
      >
        Open PR
      </Button>
    </div>
  );
}

// PullRow renders one PR with its CI dot, a Merge action for open PRs, and an
// expandable run list (current runs + recent history, lazily fetched on first
// expand).
function PullRow({
  issueID,
  pr,
  confirm,
  onMerged,
}: {
  issueID: string;
  pr: PullRef;
  confirm: (o: ConfirmOptions) => Promise<boolean>;
  onMerged: (merged: PullRef) => void | Promise<void>;
}) {
  const [expanded, setExpanded] = useState(false);
  const [runs, setRuns] = useState<CIRun[] | null>(null);
  const [ciState, setCIState] = useState<string>("");
  const [method, setMethod] = useState<MergeMethod>("merge");
  const ciAction = useAsyncAction();
  const mergeAction = useAsyncAction();

  const loadCI = async () => {
    await ciAction.run(async () => {
      const { status, history } = await getIssuePullCI(issueID, pr.number);
      setCIState(status.state);
      // Current runs first, then recent history; de-dupe is the server's job.
      setRuns([...(status.runs ?? []), ...(history ?? [])]);
    });
  };

  const toggle = () => {
    const next = !expanded;
    setExpanded(next);
    if (next && runs === null && !ciAction.busy) void loadCI();
  };

  // Open PRs (not merged/closed/draft) can be merged. The forge enforces the
  // real merge gate (CI, approvals) — this is the operator affordance.
  const mergeable = ["open", "opened"].includes(pr.state.toLowerCase()) && !pr.draft;

  const doMerge = async () => {
    const ok = await confirm({
      title: `Merge PR #${pr.number}?`,
      message: `Merge ${pr.source_branch} → ${pr.target_branch} using "${method}"? This cannot be undone.`,
      confirmLabel: "Merge",
    });
    if (!ok) return;
    const merged = await mergeAction.run(() =>
      mergeIssuePull(issueID, pr.number, { method }),
    );
    if (merged) await onMerged(merged);
  };

  return (
    <div className="border-t border-border-subtle pt-1.5 first:border-t-0 first:pt-0">
      <div className="flex items-center gap-2 text-xs">
        <CIDot state={ciState} />
        <a
          href={pr.url}
          target="_blank"
          rel="noreferrer"
          className="font-medium text-accent-text hover:underline truncate"
          title={pr.title}
        >
          #{pr.number} {pr.title}
        </a>
        <Badge variant={prStateVariant(pr.state)} size="sm">
          {pr.draft ? "draft" : pr.state}
        </Badge>
      </div>
      <div className="mt-0.5 flex items-center gap-2 text-micro text-fg-subtle">
        <span className="font-mono truncate">
          {pr.source_branch} → {pr.target_branch}
        </span>
        <button
          type="button"
          onClick={toggle}
          className="ml-auto text-accent-text hover:underline"
        >
          {expanded ? "Hide CI" : "CI"}
        </button>
      </div>
      {mergeable && (
        <div className="mt-1 flex items-center gap-2">
          <Select
            size="sm"
            fit
            value={method}
            onChange={(e) => setMethod(e.target.value as MergeMethod)}
            disabled={mergeAction.busy}
            aria-label="Merge method"
          >
            <option value="merge">merge</option>
            <option value="squash">squash</option>
            <option value="rebase">rebase</option>
          </Select>
          <Button
            size="sm"
            onClick={() => void doMerge()}
            loading={mergeAction.busy}
            disabled={mergeAction.busy}
          >
            Merge
          </Button>
        </div>
      )}
      {mergeAction.error && (
        <InlineBanner tone="danger" layout="inline" className="mt-1">
          {mergeAction.error}
        </InlineBanner>
      )}
      {expanded && (
        <div className="mt-1 space-y-1">
          {ciAction.busy && (
            <p className="text-micro text-fg-subtle italic">Loading CI runs…</p>
          )}
          {ciAction.error && (
            <InlineBanner tone="danger" layout="inline">
              {ciAction.error}
            </InlineBanner>
          )}
          {runs && runs.length === 0 && !ciAction.busy && (
            <p className="text-micro text-fg-subtle">No CI runs reported.</p>
          )}
          {(runs ?? []).map((run, i) => (
            <div key={`${run.name}-${run.sha}-${i}`} className="flex items-center gap-2 text-micro">
              <Badge variant={ciRunVariant(run)} size="sm">
                {run.conclusion || run.status}
              </Badge>
              {run.url ? (
                <a
                  href={run.url}
                  target="_blank"
                  rel="noreferrer"
                  className="text-accent-text hover:underline truncate"
                >
                  {run.name}
                </a>
              ) : (
                <span className="text-fg-default truncate">{run.name}</span>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// CIDot renders the aggregate CI state as a coloured dot:
// success=green, failed=red, running/pending=amber, unknown=grey.
function CIDot({ state }: { state: string }) {
  const tone = ciTone(state);
  const cls: Record<typeof tone, string> = {
    success: "bg-success",
    danger: "bg-danger",
    warning: "bg-warning",
    neutral: "bg-fg-subtle",
  };
  return (
    <span
      className={`inline-block h-2 w-2 rounded-full shrink-0 ${cls[tone]}`}
      title={state ? `CI: ${state}` : "CI status unknown"}
      aria-label={state ? `CI ${state}` : "CI status unknown"}
    />
  );
}

type CITone = "success" | "danger" | "warning" | "neutral";

// ciTone maps a forge CI aggregate state to a semantic tone. Covers the
// common GitHub/GitLab/Forgejo vocabularies (success/passed, failure/error,
// running/pending/in_progress).
function ciTone(state: string): CITone {
  const s = state.toLowerCase();
  if (["success", "passed", "completed", "ok"].includes(s)) return "success";
  if (["failure", "failed", "error", "cancelled", "canceled"].includes(s)) return "danger";
  if (["running", "pending", "in_progress", "queued", "waiting"].includes(s)) return "warning";
  return "neutral";
}

function ciToneToBadge(tone: CITone): BadgeVariant {
  switch (tone) {
    case "success":
      return "success";
    case "danger":
      return "danger";
    case "warning":
      return "warning";
    default:
      return "neutral";
  }
}

// ciRunVariant prefers the run's conclusion (terminal) over its status
// (lifecycle) when colouring the per-run badge.
function ciRunVariant(run: CIRun): BadgeVariant {
  return ciToneToBadge(ciTone(run.conclusion || run.status));
}

// prStateVariant maps a PR state to a badge variant (open=success,
// merged=accent, closed=neutral).
function prStateVariant(state: string): BadgeVariant {
  const s = state.toLowerCase();
  if (s === "merged") return "accent";
  if (s === "open" || s === "opened") return "success";
  return "neutral";
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
