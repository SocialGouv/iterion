import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useLocation } from "wouter";
import { InlineBanner } from "@/components/ui/InlineBanner";

import {
  FeatureUnavailableError,
  effectiveKind,
  type BindingKind,
  type BotSecretBinding,
  createBinding,
  deleteBinding,
  listBindings,
  updateBinding,
} from "@/api/botBindings";
import { type GenericSecretView, listTeamSecrets } from "@/api/secrets";

import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { TagInput } from "@/components/ui/TagInput";
import ConfirmDialog from "@/components/shared/ConfirmDialog";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useBotsStore } from "@/store/bots";

interface Props {
  teamID: string;
  canManage: boolean;
}

export default function BindingsTab({ teamID, canManage }: Props) {
  // Bots come from the shared catalog cache so a manifest edit elsewhere
  // re-renders the picker. We default `activeBot` to the first entry the
  // first time the catalog resolves.
  const bots = useBotsStore((s) => s.bots) ?? [];
  const botsLoading = useBotsStore((s) => s.loading);
  const fetchBots = useBotsStore((s) => s.fetch);
  const queryClient = useQueryClient();
  const [activeBot, setActiveBot] = useState<string>("");
  // Mutation failures report through setActionErr; load failures surface
  // from the bindings query. One banner shows whichever is current.
  const [actionErr, setActionErr] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<BotSecretBinding | null>(null);
  const [deleting, setDeleting] = useState<BotSecretBinding | null>(null);
  const [, navigate] = useLocation();

  useEffect(() => {
    void fetchBots();
  }, [fetchBots]);

  // Pick the first bot once the catalog resolves. Guarded against a
  // user-set value so we don't clobber a manual pick on later re-renders.
  useEffect(() => {
    if (bots.length > 0 && activeBot === "") setActiveBot(bots[0]!.name);
  }, [bots, activeBot]);

  // Team secrets feed the create dialog's picker; a failure deliberately
  // collapses to an empty list (the empty state nudges toward Secrets).
  const secretsQuery = useQuery({
    queryKey: ["team-secrets", teamID],
    queryFn: () => listTeamSecrets(teamID),
  });
  const secrets = secretsQuery.data ?? [];
  const secretsLoading = secretsQuery.isLoading;

  const bindingsQuery = useQuery({
    queryKey: ["bot-bindings", teamID, activeBot],
    queryFn: () => listBindings(teamID, activeBot),
    enabled: activeBot !== "",
  });
  const bindings = bindingsQuery.data ?? [];
  // isFetching (not isLoading): the pre-query code skeletoned the table
  // on every reload, including post-mutation refreshes.
  const loading = bindingsQuery.isFetching;
  const unavailable = bindingsQuery.error instanceof FeatureUnavailableError;
  const err =
    actionErr ??
    (bindingsQuery.error && !unavailable && !loading
      ? errorMessage(bindingsQuery.error)
      : null);

  const reload = () => {
    setActionErr(null);
    void queryClient.invalidateQueries({ queryKey: ["bot-bindings", teamID] });
  };

  const doDelete = async () => {
    if (!deleting) return;
    try {
      await deleteBinding(teamID, deleting.bot_id, deleting.id);
      setDeleting(null);
      reload();
    } catch (e) {
      setActionErr(errorMessage(e));
    }
  };

  if (unavailable) {
    return (
      <EmptyState
        title="Bot bindings not enabled on this server"
        message="Bot-secret bindings require a multi-tenant deployment."
      />
    );
  }

  const noBots = !botsLoading && bots.length === 0;
  const noSecrets = !secretsLoading && secrets.length === 0;

  return (
    <div className="space-y-4">
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      <div className="flex flex-wrap items-end justify-between gap-2">
        <div>
          <div className="font-medium">Bot bindings</div>
          <p className="text-xs text-fg-subtle">
            Map a team secret to the name a bot's workflow declares in its <code>secrets:</code>{" "}
            block (optionally narrowing the egress hosts) — or pin a context document (a file in a
            git repo) to a name its <code>attachments:</code> block declares, delivered to every
            run of the bot.
          </p>
          <p className="text-caption text-fg-subtle mt-0.5">
            Values come from{" "}
            <button
              type="button"
              className="text-accent-text hover:underline"
              onClick={() => navigate("/integrations?tab=secrets")}
            >
              Secrets
            </button>
            .
          </p>
        </div>
        <div className="flex items-center gap-2">
          <label className="text-xs text-fg-muted">Bot:</label>
          <Select
            value={activeBot}
            onChange={(e) => {
              // A stale mutation error doesn't apply to the newly-picked bot.
              setActionErr(null);
              setActiveBot(e.target.value);
            }}
            disabled={noBots}
          >
            <option value="" disabled>
              — select a bot —
            </option>
            {bots.map((b) => (
              <option key={b.name} value={b.name}>
                {b.display_name ? `${b.display_name} (${b.name})` : b.name}
              </option>
            ))}
          </Select>
          {canManage && activeBot && (
            <Button
              size="sm"
              variant="primary"
              onClick={() => setCreating(true)}
              title={noSecrets ? "A secret binding needs a team secret first — a context binding does not" : undefined}
            >
              Add binding
            </Button>
          )}
        </div>
      </div>

      {noSecrets && bindings.length === 0 && (
        <EmptyState
          title="No team secrets yet"
          message="A secret binding exposes a team secret to a bot under a chosen workflow name — add a secret first for that kind. A context binding (a pinned document) needs none."
          action={
            canManage ? (
              <Button
                size="sm"
                variant="primary"
                onClick={() => navigate("/integrations?tab=secrets")}
              >
                Go to Secrets
              </Button>
            ) : undefined
          }
        />
      )}

      {(noBots ? (
          <EmptyState
            title="No bots in your catalog yet"
            message="Bindings target bots discovered from your workspace or the marketplace. Once a bot exists on this server, pick it above to manage its bindings."
            action={
              <Button size="sm" variant="secondary" onClick={() => navigate("/bots")}>
                Open the Bots catalog
              </Button>
            }
          />
        ) : !activeBot ? (
          <EmptyState message="Pick a bot to view its bindings." />
        ) : loading ? (
          <TableSkeleton rows={4} cols={6} />
        ) : bindings.length === 0 ? (
          <EmptyState
            message={
              canManage
                ? "No bindings for this bot. Add one to expose a team secret or a pinned context document to the workflow."
                : "No bindings for this bot. Ask an admin to add one."
            }
          />
        ) : (
          <Table caption={`Bindings for ${activeBot}`}>
            <THead>
              <Th>Workflow name</Th>
              <Th>Kind</Th>
              <Th>Source</Th>
              <Th>Allowed hosts</Th>
              <Th>Updated</Th>
              <Th align="right">Actions</Th>
            </THead>
            <TBody>
              {bindings.map((b) => {
                const kind = effectiveKind(b);
                const sec = secrets.find((s) => s.id === b.secret_id);
                const secretMissing = kind === "secret" && !sec;
                return (
                  <Tr key={b.id}>
                    <Td className="font-mono">
                      {b.secret_name_for_workflow}
                      {b.degraded_reason && (
                        <div className="mt-1">
                          <Badge variant="warning" title={b.degraded_reason}>
                            degraded
                          </Badge>
                          <div className="text-caption text-fg-subtle mt-0.5 max-w-md break-words">
                            {b.degraded_reason}
                          </div>
                        </div>
                      )}
                    </Td>
                    <Td>
                      <Badge variant={kind === "context" ? "info" : "neutral"}>{kind}</Badge>
                    </Td>
                    <Td>
                      {kind === "context" ? (
                        b.context ? (
                          <div className="text-xs">
                            <div className="font-mono break-all">
                              {b.context.git_url} <span className="text-fg-muted">@</span>{" "}
                              {b.context.ref}
                            </div>
                            <div className="text-caption text-fg-subtle font-mono break-all">
                              {b.context.path}
                            </div>
                          </div>
                        ) : (
                          <span className="text-danger text-xs">No pinned source</span>
                        )
                      ) : sec ? (
                        <span>
                          {sec.name}{" "}
                          <span className="text-fg-muted">…{sec.last4 ?? "????"}</span>
                        </span>
                      ) : (
                        <div className="space-y-1">
                          <div className="text-danger text-xs">
                            Bound secret was deleted
                          </div>
                          <div className="text-caption text-fg-subtle font-mono break-all">
                            id: {b.secret_id}
                          </div>
                          {canManage && (
                            <div className="text-caption">
                              <button
                                type="button"
                                className="text-accent-text hover:underline"
                                onClick={() => setEditing(b)}
                              >
                                Pick a replacement
                              </button>
                              <span className="text-fg-subtle"> · </span>
                              <button
                                type="button"
                                className="text-danger hover:underline"
                                onClick={() => setDeleting(b)}
                              >
                                Remove binding
                              </button>
                            </div>
                          )}
                        </div>
                      )}
                    </Td>
                    <Td className="text-xs">
                      {(b.allowed_hosts ?? []).length === 0 ? (
                        <span className="text-fg-muted">workflow default</span>
                      ) : (
                        (b.allowed_hosts ?? []).map((h) => (
                          <span
                            key={h}
                            className="inline-block bg-surface-2 rounded px-1 mr-1"
                          >
                            {h}
                          </span>
                        ))
                      )}
                    </Td>
                    <Td className="text-fg-muted text-xs">
                      {formatDateTime(b.updated_at)}
                    </Td>
                    <Td align="right" className="space-x-1 whitespace-nowrap">
                      {canManage && (
                        <>
                          <Button
                            size="sm"
                            variant={secretMissing ? "primary" : "ghost"}
                            onClick={() => setEditing(b)}
                          >
                            Edit
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            className="text-danger"
                            onClick={() => setDeleting(b)}
                          >
                            Delete
                          </Button>
                        </>
                      )}
                    </Td>
                  </Tr>
                );
              })}
            </TBody>
          </Table>
        ))}

      {(creating || editing) && activeBot && (
        <BindingDialog
          teamID={teamID}
          botID={activeBot}
          secrets={secrets}
          initial={editing}
          onClose={() => {
            setCreating(false);
            setEditing(null);
          }}
          onSaved={() => {
            setCreating(false);
            setEditing(null);
            reload();
          }}
        />
      )}

      <ConfirmDialog
        open={deleting !== null}
        title="Delete binding?"
        message="The bot's workflow will no longer resolve this name. Workflows that rely on it will fail until you re-bind it."
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={() => void doDelete()}
        onCancel={() => setDeleting(null)}
      />
    </div>
  );
}

function BindingDialog({
  teamID,
  botID,
  secrets,
  initial,
  onClose,
  onSaved,
}: {
  teamID: string;
  botID: string;
  secrets: GenericSecretView[];
  initial: BotSecretBinding | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [kind, setKind] = useState<BindingKind>(initial ? effectiveKind(initial) : "secret");
  const [secretID, setSecretID] = useState(initial?.secret_id ?? "");
  const [name, setName] = useState(initial?.secret_name_for_workflow ?? "");
  const [hosts, setHosts] = useState<string[]>(initial?.allowed_hosts ?? []);
  const [gitURL, setGitURL] = useState(initial?.context?.git_url ?? "");
  const [ref, setRef] = useState(initial?.context?.ref ?? "");
  const [path, setPath] = useState(initial?.context?.path ?? "");
  const [readCredID, setReadCredID] = useState(initial?.context?.secret_id ?? "");
  const { busy, error: err, run } = useAsyncAction();

  const submit = () =>
    run(async () => {
      if (kind === "context") {
        const context = {
          git_url: gitURL.trim(),
          ref: ref.trim(),
          path: path.trim(),
          secret_id: readCredID || undefined,
        };
        if (initial) {
          await updateBinding(teamID, botID, initial.id, {
            secret_name_for_workflow: name,
            context,
          });
        } else {
          await createBinding(teamID, botID, {
            kind: "context",
            secret_name_for_workflow: name,
            context,
          });
        }
      } else if (initial) {
        await updateBinding(teamID, botID, initial.id, {
          secret_id: secretID,
          secret_name_for_workflow: name,
          allowed_hosts: hosts,
        });
      } else {
        await createBinding(teamID, botID, {
          kind: "secret",
          secret_id: secretID,
          secret_name_for_workflow: name,
          allowed_hosts: hosts.length ? hosts : undefined,
        });
      }
      onSaved();
    });

  const valid =
    name.trim() !== "" &&
    (kind === "secret"
      ? secretID !== ""
      : gitURL.trim() !== "" && ref.trim() !== "" && path.trim() !== "");

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={initial ? "Edit binding" : "New binding"}
      widthClass="max-w-lg"
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!valid} loading={busy} onClick={() => void submit()}>
            {initial ? "Save" : "Create"}
          </Button>
        </>
      }
    >
      {err && (
        <InlineBanner tone="danger" layout="inline" className="mb-3">
          {err}
        </InlineBanner>
      )}
      {initial && kind === "secret" && !secrets.some((s) => s.id === initial.secret_id) && (
        <InlineBanner tone="warning" layout="inline" className="mb-3">
          The secret behind this binding was deleted. Pick a replacement below (previous id:{" "}
          <span className="font-mono break-all">{initial.secret_id}</span>) or cancel and remove
          the binding.
        </InlineBanner>
      )}
      <div className="space-y-3 text-sm">
        <label className="block">
          <div className="text-xs text-fg-muted mb-1">Kind</div>
          {initial ? (
            // The kind is immutable: the update route never applies a kind
            // change, so the edit form shows it rather than offering one.
            <div>
              <Badge variant={kind === "context" ? "info" : "neutral"}>{kind}</Badge>
            </div>
          ) : (
            <Select value={kind} onChange={(e) => setKind(e.target.value as BindingKind)}>
              <option value="secret">secret — a team credential exposed to the workflow</option>
              <option value="context">context — a pinned document delivered to every run</option>
            </Select>
          )}
        </label>
        {kind === "secret" ? (
          <label className="block">
            <div className="text-xs text-fg-muted mb-1">Team secret</div>
            <Select value={secretID} onChange={(e) => setSecretID(e.target.value)}>
              <option value="">— pick a secret —</option>
              {secrets.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name} (…{s.last4 ?? "????"})
                </option>
              ))}
            </Select>
          </label>
        ) : (
          <>
            <label className="block">
              <div className="text-xs text-fg-muted mb-1">Source repository</div>
              <Input
                value={gitURL}
                onChange={(e) => setGitURL(e.target.value)}
                placeholder="https://gitlab.example.com/org/bot-context.git"
              />
            </label>
            <label className="block">
              <div className="text-xs text-fg-muted mb-1">Ref (pin a tag or sha)</div>
              <Input value={ref} onChange={(e) => setRef(e.target.value)} placeholder="v1.4.2" />
              <div className="text-caption text-fg-subtle mt-1">
                A moving branch makes the document change under the operator's feet.
              </div>
            </label>
            <label className="block">
              <div className="text-xs text-fg-muted mb-1">Path in the source</div>
              <Input
                value={path}
                onChange={(e) => setPath(e.target.value)}
                placeholder="handbook/pricing.pdf"
              />
            </label>
            <label className="block">
              <div className="text-xs text-fg-muted mb-1">Read credential (optional)</div>
              <Select value={readCredID} onChange={(e) => setReadCredID(e.target.value)}>
                <option value="">— public repository —</option>
                {secrets.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name} (…{s.last4 ?? "????"})
                  </option>
                ))}
              </Select>
              <div className="text-caption text-fg-subtle mt-1">
                A team secret (PAT or deploy key) used only to READ the source; it is never
                exposed to the workflow.
              </div>
            </label>
          </>
        )}
        <label className="block">
          <div className="text-xs text-fg-muted mb-1">Workflow name</div>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={kind === "secret" ? "forge_token" : "handbook"}
          />
          <div className="text-caption text-fg-subtle mt-1">
            {kind === "secret" ? (
              <>
                The name the bot's workflow declares in its <code>secrets:</code> block. Must
                match exactly.
              </>
            ) : (
              <>
                The name the bot's workflow declares in its <code>attachments:</code> block — the
                document arrives as the run attachment of that name. Must match exactly.
              </>
            )}
          </div>
        </label>
        {kind === "secret" && (
          <label className="block">
            <div className="text-xs text-fg-muted mb-1">Allowed egress hosts (optional)</div>
            <TagInput value={hosts} onChange={setHosts} placeholder="gitlab.example.com" />
            <div className="text-caption text-fg-subtle mt-1">
              If set, these hosts intersect (never broaden) the workflow's declared{" "}
              <code>hosts:</code>. Leave empty to keep the workflow default.
            </div>
          </label>
        )}
      </div>
    </Dialog>
  );
}
