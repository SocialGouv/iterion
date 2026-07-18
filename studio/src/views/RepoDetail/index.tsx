import { useCallback, useEffect, useMemo, useState } from "react";

import { useQueryClient } from "@tanstack/react-query";
import { Link, useParams } from "wouter";

import { FeatureUnavailableError } from "@/api/client";
import {
  disableForgeIntegration,
  updateForgeRepoBots,
  forgeTeamRepoKey,
  listForgeIntegrations,
  type ForgeIntegration,
  type ForgeTeamRepo,
} from "@/api/forgeConnections";
import { listTeamSchedules, type ScheduledBot } from "@/api/schedules";
import { listTriggers, type TriggerSubscription } from "@/api/triggers";
import BotIdentity from "@/components/shared/BotIdentity";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import {
  Badge,
  Button,
  Card,
  EmptyState,
  InlineBanner,
  Spinner,
} from "@/components/ui";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useCanManageTeam } from "@/hooks/useCanManageTeam";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
import { forgeLabel } from "@/lib/forge";
import { humanizeCron } from "@/lib/humanizeCron";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";
import { botLaunchFile } from "@/views/Bots/botPaths";
import { bindBotPath } from "@/views/integrations/wizard/bindModel";
import { formatNextFire } from "@/views/Triggers/scheduleModel";
import TriggerList from "@/views/Triggers/TriggerList";

import {
  integrationForRepo,
  repoSummary,
  schedulesForRepo,
  triggersForRepo,
} from "./model";
import { decodeRepoKey, repoDetailPath } from "./repoKey";

/**
 * RepoDetailView — /repos/:key — one connected repository's home:
 * identity + connection health, the bots bound to it (with their
 * webhook events and launch/unbind actions), its schedules, and its
 * repo-scoped automations. The key is the RepoSwitcher's stable repo
 * identity (forgeTeamRepoKey), URL-encoded as one path segment.
 */
export default function RepoDetailView() {
  const params = useParams<{ key: string }>();
  const key = decodeRepoKey(params.key ?? "");
  const { repos, loading, enabled, teamID } = useActiveRepo();
  const repo = useMemo(
    () => repos.find((r) => forgeTeamRepoKey(r) === key) ?? null,
    [repos, key],
  );

  useHeaderSlot({
    left: (
      <span className="flex items-center gap-1.5 text-xs font-medium text-fg-default">
        <Link
          href="/integrations"
          className="text-fg-muted hover:text-fg-default hover:underline"
        >
          Integrations
        </Link>
        <span className="text-fg-subtle">/</span>
        <span className="font-mono">{repo?.repo_full_name ?? "Repository"}</span>
      </span>
    ),
  });

  if (!enabled) {
    return (
      <div className="p-6">
        <EmptyState
          title="No connected repositories"
          message="Connected repositories are a cloud feature — sign in to a cloud team to bind bots to a repo."
        />
      </div>
    );
  }
  if (!repo) {
    if (loading) {
      return (
        <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
          <Spinner /> Loading repository…
        </div>
      );
    }
    return (
      <div className="p-6">
        <EmptyState
          title="Repository not connected"
          message="This repository isn't connected to the team (it may have been disconnected)."
          action={
            <Link href="/integrations">
              <Button variant="secondary" size="sm">
                Open Integrations
              </Button>
            </Link>
          }
        />
      </div>
    );
  }
  return <RepoDetail repo={repo} teamID={teamID ?? ""} />;
}

function connectionStatusMeta(
  status: ForgeTeamRepo["connection_status"],
): { tone: "success" | "warning" | "danger"; label: string } | null {
  if (!status) return null;
  if (status === "active") return { tone: "success", label: "connection active" };
  if (status === "needs_reauth" || status === "degraded") {
    return { tone: "warning", label: `connection ${status}` };
  }
  return { tone: "danger", label: `connection ${status}` };
}

function RepoDetail({ repo, teamID }: { repo: ForgeTeamRepo; teamID: string }) {
  const canManage = useCanManageTeam();
  const { confirm, dialog } = useConfirm();
  const action = useAsyncAction();
  const qc = useQueryClient();

  const bots = useBotsStore((s) => s.bots);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    void fetchBots();
  }, [fetchBots]);

  const [integrations, setIntegrations] = useState<ForgeIntegration[] | null>(null);
  const [schedules, setSchedules] = useState<ScheduledBot[] | null>(null);
  const [subs, setSubs] = useState<TriggerSubscription[] | null>(null);
  const [triggersUnavailable, setTriggersUnavailable] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const repoFullName = repo.repo_full_name;
  const triggersEnabled = useServerInfoStore((s) => s.info?.triggers_enabled ?? false);
  const reload = useCallback(async () => {
    if (!teamID) return;
    setErr(null);
    const results = await Promise.allSettled([
      listForgeIntegrations(teamID),
      listTeamSchedules(teamID),
      // Skip the round-trip (and its console 404) when the server
      // advertises no trigger store.
      triggersEnabled
        ? listTriggers({ repo: repoFullName })
        : Promise.reject(new FeatureUnavailableError("triggers")),
    ]);
    const [ints, scheds, triggers] = results;
    const failures: string[] = [];
    const settle = <T,>(
      r: PromiseSettledResult<T>,
      apply: (v: T) => void,
      onUnavailable: () => void,
    ) => {
      if (r.status === "fulfilled") {
        apply(r.value);
      } else if (r.reason instanceof FeatureUnavailableError) {
        onUnavailable();
      } else {
        failures.push(errorMessage(r.reason));
      }
    };
    settle(ints, setIntegrations, () => setIntegrations([]));
    settle(scheds, setSchedules, () => setSchedules([]));
    settle(triggers, setSubs, () => setTriggersUnavailable(true));
    if (failures.length > 0) setErr(failures.join(" · "));
  }, [teamID, repoFullName, triggersEnabled]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const integration = useMemo(
    () => integrationForRepo(integrations ?? [], repo),
    [integrations, repo],
  );
  const repoSchedules = useMemo(
    () => schedulesForRepo(schedules ?? [], repo),
    [schedules, repo],
  );
  const repoTriggers = useMemo(
    () => triggersForRepo(subs ?? [], repo.repo_full_name),
    [subs, repo.repo_full_name],
  );

  const botEntryFor = useCallback(
    (id: string) => (bots ?? []).find((b) => b.name === id),
    [bots],
  );
  const botLabelFor = useCallback(
    (id: string) => botEntryFor(id)?.display_name?.trim() || id,
    [botEntryFor],
  );

  // Per-bot unbind: PATCH the integration down to the remaining bots
  // (replace semantics — webhook events and schedules narrow with it).
  // Removing the last bot deprovisions the whole integration, which also
  // tears down the forge webhook; the confirm says which one happens.
  const unbind = async (botId: string) => {
    if (!integration) return;
    const remaining = integration.bot_ids.filter((b) => b !== botId);
    const ok = await confirm({
      title: `Unbind ${botLabelFor(botId)}?`,
      message:
        remaining.length > 0
          ? `${botLabelFor(botId)} stops reacting to ${repo.repo_full_name}. The ${remaining.length === 1 ? "other bot stays" : `${remaining.length} other bots stay`} bound.`
          : `${botLabelFor(botId)} is the last bot on ${repo.repo_full_name} — this also removes the iterion webhook from the repository.`,
      confirmLabel: "Unbind",
      confirmVariant: "danger",
    });
    if (!ok) return;
    const done = await action.run(async () => {
      if (remaining.length > 0) {
        await updateForgeRepoBots(teamID, integration.id, remaining);
      } else {
        await disableForgeIntegration(teamID, integration.id);
      }
      return true;
    });
    if (done) {
      await reload();
      // Refresh the connected-repos aggregator (RepoSwitcher, Home, this
      // view's own not-found fallback) so the bot counts / unbound repo
      // update everywhere.
      void qc.invalidateQueries({ queryKey: ["team-forge-repos", teamID] });
    }
  };

  const wording = forgeLabel(repo.provider);
  const status = connectionStatusMeta(repo.connection_status);
  const events = integration?.events_normalized ?? [];
  const scheduleManageHref = `/triggers?tab=schedules&repo=${encodeURIComponent(repo.repo_full_name)}`;
  const bindHref = bindBotPath({
    repoKey: forgeTeamRepoKey(repo),
    returnTo: repoDetailPath(repo),
  });

  return (
    <div className="mx-auto flex w-full max-w-3xl flex-col gap-4 p-4">
      <header>
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="font-mono text-base font-semibold text-fg-default">
            {repo.repo_full_name}
          </h1>
          <Badge variant="neutral" size="sm">
            {repo.provider} · {wording.noun}s
          </Badge>
          {status && (
            <InlineBanner tone={status.tone} layout="inline">
              {status.label}
            </InlineBanner>
          )}
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-fg-muted">
          <span>{repoSummary(repo.bot_ids.length, repoSchedules.length)}</span>
          {repo.web_url && (
            <a
              href={repo.web_url}
              target="_blank"
              rel="noreferrer"
              className="text-accent-text hover:underline"
            >
              Open on {repo.provider} ↗
            </a>
          )}
        </div>
      </header>

      {err && (
        <InlineBanner tone="danger" title="Couldn't load some repo data">
          {err}
        </InlineBanner>
      )}
      {action.error && (
        <InlineBanner tone="danger" title="Action failed">
          {action.error}
        </InlineBanner>
      )}

      {/* Bound bots */}
      <Card flush>
        <header className="flex items-center justify-between border-b border-border-subtle px-4 py-2.5">
          <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
            Bound bots
          </h2>
          <Link href={bindHref}>
            <Button variant="primary" size="sm">
              Bind a bot
            </Button>
          </Link>
        </header>
        {events.length > 0 && (
          <div className="flex flex-wrap items-center gap-1.5 border-b border-border-subtle px-4 py-2 text-caption text-fg-muted">
            <span>Webhook events:</span>
            {events.map((e) => (
              <Badge key={e} variant="neutral" size="sm">
                {e}
              </Badge>
            ))}
          </div>
        )}
        {repo.bot_ids.length === 0 ? (
          <EmptyState
            message={
              <>
                No bot is bound to this repository yet. Bind one so a{" "}
                {wording.long} or an issue can put it to work.
              </>
            }
          />
        ) : (
          <ul className="divide-y divide-border-subtle">
            {repo.bot_ids.map((id) => {
              const entry = botEntryFor(id);
              const file = entry ? botLaunchFile(entry) : null;
              return (
                <li
                  key={id}
                  className="flex flex-wrap items-center gap-3 px-4 py-2.5"
                >
                  <BotIdentity
                    bot={entry ?? { name: id }}
                    clampDescription
                    className="min-w-0 flex-1"
                  />
                  <div className="flex shrink-0 items-center gap-1.5">
                    {file && (
                      <Link href={`/runs/new?file=${encodeURIComponent(file)}`}>
                        <Button variant="secondary" size="sm">
                          Launch
                        </Button>
                      </Link>
                    )}
                    <Link href={`/bots/${encodeURIComponent(id)}`}>
                      <Button variant="ghost" size="sm">
                        Open bot
                      </Button>
                    </Link>
                    {canManage && integration && (
                      <Button
                        variant="danger"
                        size="sm"
                        onClick={() => void unbind(id)}
                        disabled={action.busy}
                      >
                        Unbind
                      </Button>
                    )}
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </Card>

      {/* Schedules */}
      <Card flush>
        <header className="flex items-center justify-between border-b border-border-subtle px-4 py-2.5">
          <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
            Schedules
          </h2>
          <Link
            href={scheduleManageHref}
            className="text-micro text-accent-text hover:underline"
          >
            Manage
          </Link>
        </header>
        {schedules === null ? (
          <div className="flex items-center gap-2 px-4 py-3 text-sm text-fg-muted">
            <Spinner /> Loading schedules…
          </div>
        ) : repoSchedules.length === 0 ? (
          <EmptyState
            message={
              <>
                No schedule targets this repository.{" "}
                <Link
                  href={scheduleManageHref}
                  className="text-accent-text hover:underline"
                >
                  Create one
                </Link>{" "}
                to run a bot on a cron.
              </>
            }
          />
        ) : (
          <ul className="divide-y divide-border-subtle">
            {repoSchedules.map((s) => (
              <li
                key={s.id}
                className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-2 text-sm"
              >
                <span className="font-medium text-fg-default">
                  {botLabelFor(s.bot_id)}
                </span>
                <span className="font-mono text-caption text-fg-muted">
                  {s.cron}
                </span>
                {humanizeCron(s.cron) && (
                  <span className="text-caption text-fg-muted">
                    {humanizeCron(s.cron)}
                  </span>
                )}
                <span className="ml-auto text-caption text-fg-subtle">
                  {s.disabled ? (
                    <Badge variant="neutral" size="sm">
                      paused
                    </Badge>
                  ) : (
                    `next ${formatNextFire(s.next_fire_at)}`
                  )}
                </span>
              </li>
            ))}
          </ul>
        )}
      </Card>

      {/* Automations */}
      <Card flush>
        <header className="flex items-center justify-between border-b border-border-subtle px-4 py-2.5">
          <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
            Automations
          </h2>
          <Link
            href="/triggers"
            className="text-micro text-accent-text hover:underline"
          >
            All automations
          </Link>
        </header>
        {triggersUnavailable ? (
          <div className="px-4 py-3 text-xs text-fg-muted">
            Event triggers aren't enabled on this server — this repo is
            automated via forge webhooks and schedules.
          </div>
        ) : subs === null ? (
          <div className="flex items-center gap-2 px-4 py-3 text-sm text-fg-muted">
            <Spinner /> Loading triggers…
          </div>
        ) : repoTriggers.length === 0 ? (
          <EmptyState
            message={
              <>
                No repo-scoped trigger yet.{" "}
                <Link href="/triggers" className="text-accent-text hover:underline">
                  Create one
                </Link>{" "}
                to launch a bot on an event.
              </>
            }
          />
        ) : (
          <div className="p-3">
            <TriggerList subs={repoTriggers} onChanged={() => void reload()} />
          </div>
        )}
      </Card>

      {dialog}
    </div>
  );
}
