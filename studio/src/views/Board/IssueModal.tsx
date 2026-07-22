import { useEffect, useMemo, useState } from "react";

import { type BotEntryWithSchema } from "@/api/bots";
import { forgeTeamRepoKey } from "@/api/forgeConnections";
import {
  type ExternalLinkInput,
  type NativeBoard,
  type NativeIssue,
} from "@/api/native";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Tabs } from "@/components/ui/Tabs";
import { defaultStringFor } from "@/components/shared/VarFieldInput";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { isVarMissing } from "@/lib/varValidation";
import { useBotsStore } from "@/store/bots";

import { ApproveTriageBanner } from "./ApproveTriage";
import { BotTab } from "./issueModal/BotTab";
import { RepositoryField } from "./issueModal/RepositoryField";
import { TicketTab } from "./issueModal/TicketTab";

// IssueDraft widens Partial<NativeIssue> for the modal's onSubmit callback:
// `external` carries the operator's picker input (no server-populated
// number/url yet), which is not assignable to NativeIssue.external.
export type IssueDraft = Partial<Omit<NativeIssue, "external">> & {
  external?: ExternalLinkInput;
};

interface Props {
  board: NativeBoard;
  initial: NativeIssue | null;
  onSubmit: (input: IssueDraft) => Promise<void> | void;
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

  // Repo-first scoping: the picker is fed by the same connected-repos list
  // the sidebar switcher uses (cloud-only, gated on `enabled`). A card
  // already synced from its forge (external.number/url present) locks the
  // picker into a read-only "synced from forge" note.
  const {
    activeRepo,
    overview,
    repos: connectedRepos,
    enabled: repoScopeEnabled,
  } = useActiveRepo();
  const initialRepoKey = useMemo(() => {
    // Existing external → match by (connection_id, repo). Falls through to
    // the forgeTeamRepoKey format even when the repo has been disconnected
    // — RepositoryField surfaces it as a legacy option so save doesn't
    // silently drop the link.
    const ex = initial?.external;
    if (ex?.connection_id && ex.repo) {
      return `${ex.connection_id}::${ex.repo}`;
    }
    // New card in cloud mode: pre-fill from the sidebar's active repo
    // (skip in overview mode — operator explicitly wants no default).
    if (!initial && repoScopeEnabled && !overview && activeRepo) {
      return forgeTeamRepoKey(activeRepo);
    }
    return "";
  }, [initial, repoScopeEnabled, overview, activeRepo]);
  const [repoKey, setRepoKey] = useState(initialRepoKey);

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
    setRepoKey(initialRepoKey);
    const out: Record<string, string> = {};
    for (const f of board.fields ?? []) {
      const v = initial?.fields?.[f.name];
      out[f.name] = v == null ? "" : String(v);
    }
    setFields(out);
  }, [initial, board, initialRepoKey]);

  const selectedBot: BotEntryWithSchema | null = useMemo(() => {
    if (!bot || !bots) return null;
    return bots.find((b) => b.name === bot) ?? null;
  }, [bot, bots]);

  // A card is "synced from forge" once the server has stamped a number or a
  // URL onto its external link — from that point re-linking would break the
  // upstream sync, so the picker locks read-only.
  const syncedExternal = useMemo(() => {
    const ex = initial?.external;
    if (!ex) return null;
    return ex.number > 0 || ex.url ? ex : null;
  }, [initial]);

  // Repository picker: only offered in cloud mode with at least one
  // connected repo (or an existing external link, so we don't hide the
  // linkage). Outside cloud mode: no repo affordance.
  const repositoryField = useMemo(() => {
    if (!repoScopeEnabled) return undefined;
    const hasAffordance =
      connectedRepos.length > 0 || syncedExternal !== null || !!initial?.external;
    if (!hasAffordance) return undefined;
    const legacyLabel = initial?.external?.repo ?? null;
    return (
      <RepositoryField
        repos={connectedRepos}
        value={repoKey}
        onChange={setRepoKey}
        synced={syncedExternal}
        legacyLinkedLabel={legacyLabel}
      />
    );
  }, [repoScopeEnabled, connectedRepos, syncedExternal, initial, repoKey]);

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
    const out: IssueDraft = {
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
    // Repo-first scoping. Only surface `external` when the picker was
    // available AND the operator picked a repo — "No repository" (empty
    // key) omits the field so the server leaves any prior link unchanged
    // (there is no clear-link semantic today). A synced card ships its
    // untouched external so the store's pointer-nil "unchanged" rule
    // preserves the sync-owned number/url/state.
    if (repositoryField) {
      if (syncedExternal) {
        // Locked picker — never re-link.
      } else if (repoKey) {
        const picked = connectedRepos.find((r) => forgeTeamRepoKey(r) === repoKey);
        if (picked) {
          out.external = {
            provider: picked.provider,
            connection_id: picked.connection_id,
            repo: picked.repo_full_name,
          };
        } else if (initial?.external && repoKey === `${initial.external.connection_id}::${initial.external.repo}`) {
          // Legacy option: keep the disconnected repo linkage intact.
          out.external = {
            provider: initial.external.provider,
            connection_id: initial.external.connection_id,
            repo: initial.external.repo,
          };
        }
      }
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
          {initial && (
            <ApproveTriageBanner
              iss={{ ...initial, labels }}
              onApproved={setLabels}
            />
          )}
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
                  repositoryField={repositoryField}
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
