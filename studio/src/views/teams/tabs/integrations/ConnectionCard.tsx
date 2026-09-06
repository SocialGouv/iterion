import { errorMessage } from "@/lib/errorHints";
import { useState } from "react";

import { ApiError } from "@/api/client";

import { Link } from "wouter";

import type { BotEntryWithSchema } from "@/api/bots";
import {
  type ForgeConnection,
  type ForgeHook,
  type ForgeIntegration,
  type ForgeSyncResult,
  applyForgeConnectionAvatar,
  deleteForgeConnection,
  disableForgeIntegration,
  forgeTeamRepoKey,
  listForgeIntegrationHooks,
  syncForgeIntegration,
  updateForgeIntegration,
} from "@/api/forgeConnections";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { formatDate } from "@/lib/format";
import { bindBotPath } from "@/views/integrations/wizard/bindModel";
import { repoDetailPath } from "@/views/RepoDetail/repoKey";

import { connectionKindLabel, connectionStatusLabel } from "./connectionLabels";
import { BRAND_LOGO_URL, type ConfirmFn, statusTone } from "./forgeShared";
import { EnableRepoPanel } from "./EnableRepoPanel";

export function ConnectionCard({
  teamID,
  conn,
  integrations,
  repoBots,
  canManage,
  onChanged,
  onError,
  confirm,
  preselectBot,
  autoOpenEnable,
  logoUploadURL,
}: {
  teamID: string;
  conn: ForgeConnection;
  integrations: ForgeIntegration[];
  repoBots: BotEntryWithSchema[];
  canManage: boolean;
  onChanged: () => void;
  onError: (m: string) => void;
  confirm: ConfirmFn;
  preselectBot?: string;
  autoOpenEnable?: boolean;
  /** github_app only: the App's settings page where its logo is uploaded
   *  by hand (GitHub has no API for it). */
  logoUploadURL?: string;
}) {
  const [enabling, setEnabling] = useState(!!autoOpenEnable);

  const disconnect = async () => {
    const ok = await confirm({
      title: "Disconnect forge?",
      message: `Disconnecting removes every webhook iterion created on ${conn.account_login ?? conn.provider} (${integrations.length} repo${integrations.length === 1 ? "" : "s"}).`,
      confirmLabel: "Disconnect",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteForgeConnection(teamID, conn.id);
      onChanged();
    } catch (e) {
      onError(errorMessage(e));
    }
  };

  const disable = async (i: ForgeIntegration) => {
    const ok = await confirm({
      title: "Disable on this repo?",
      message: `Remove the iterion webhook from ${i.repo_full_name}?`,
      confirmLabel: "Disable",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await disableForgeIntegration(teamID, i.id);
      onChanged();
    } catch (e) {
      onError(errorMessage(e));
    }
  };

  return (
    <section className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3">
      <div className="flex items-start justify-between gap-2">
        <div>
          <div className="font-medium">
            {conn.provider} · @{conn.account_login ?? "—"}
            <InlineBanner tone={statusTone(conn.status)} layout="inline" className="ml-2 inline-flex">
              {connectionStatusLabel(conn.status)}
            </InlineBanner>
          </div>
          <div className="text-xs text-fg-muted">
            {conn.forge_base_url ?? conn.provider} · {connectionKindLabel(conn.kind)}
          </div>
          {conn.purpose === "security_read" && (
            // The one place the role must be VISIBLE rather than filtered out:
            // this connection is absent from every target picker, and without
            // saying why, that absence reads as a bug.
            <Badge variant="neutral" size="sm" className="mt-1">
              Watch-only · Dependabot alerts
            </Badge>
          )}
        </div>
        {canManage && (
          <Button
            variant="danger"
            size="sm"
            onClick={disconnect}
          >
            Disconnect
          </Button>
        )}
      </div>

      <AvatarRow
        teamID={teamID}
        conn={conn}
        canManage={canManage}
        logoUploadURL={logoUploadURL}
        onChanged={onChanged}
        onError={onError}
        confirm={confirm}
      />

      <div>
        <div className="text-xs uppercase tracking-wider text-fg-muted mb-1">Enabled repos</div>
        {integrations.length === 0 ? (
          <div className="text-fg-muted text-sm">None yet.</div>
        ) : (
          <ul className="space-y-1">
            {integrations.map((i) => (
              <RepoRow
                key={i.id}
                teamID={teamID}
                integration={i}
                canManage={canManage}
                onChanged={onChanged}
                onDisable={() => disable(i)}
                onError={onError}
              />
            ))}
          </ul>
        )}
      </div>

      {canManage &&
        (enabling ? (
          <EnableRepoPanel
            teamID={teamID}
            conn={conn}
            repoBots={repoBots}
            preselectBot={preselectBot}
            onDone={() => {
              setEnabling(false);
              onChanged();
            }}
            onCancel={() => setEnabling(false)}
            onError={onError}
          />
        ) : (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setEnabling(true)}
          >
            + Enable a repo
          </Button>
        ))}
    </section>
  );
}

// AvatarRow is the connection's face: whether the account behind it wears the
// iterion-bot avatar, and the action to give it one. Only a PAT connection on
// GitLab/Forgejo can be branded through iterion — a bot identity gets it at
// connect time; a dedicated account the forge does not flag as a bot needs the
// operator's word, hence the confirmation before `force`. An OAuth connection
// is a person's account and shows nothing; GitHub has no API, so a github_app
// connection links to the manual upload instead.
function AvatarRow({
  teamID,
  conn,
  canManage,
  logoUploadURL,
  onChanged,
  onError,
  confirm,
}: {
  teamID: string;
  conn: ForgeConnection;
  canManage: boolean;
  logoUploadURL?: string;
  onChanged: () => void;
  onError: (m: string) => void;
  confirm: ConfirmFn;
}) {
  const [busy, setBusy] = useState(false);
  if (conn.kind === "oauth_app") return null;
  if (conn.provider === "github") {
    if (conn.kind !== "github_app") return null;
    return (
      <div className="flex flex-wrap items-center gap-3 text-caption text-fg-muted">
        <span>App logo: a manual upload on GitHub (no API for it).</span>
        {logoUploadURL && (
          <a
            href={logoUploadURL}
            target="_blank"
            rel="noreferrer"
            className="text-accent-text hover:underline"
          >
            Upload the iterion-bot logo ↗
          </a>
        )}
        <a href={BRAND_LOGO_URL} download="iterion-bot.png" className="text-accent-text hover:underline">
          Download the logo
        </a>
      </div>
    );
  }

  const isBot = conn.account_kind === "bot";
  const vouch = () =>
    confirm({
      title: "Rebrand this account?",
      message: (
        <span>
          {conn.forge_base_url ?? conn.provider} does not flag{" "}
          <span className="font-mono">@{conn.account_login}</span> as a bot. Apply the
          iterion-bot avatar only if this is a dedicated account for iterion — never a
          person&apos;s.
        </span>
      ),
      confirmLabel: "Apply the avatar",
    });
  // A refusal still taught the server something (the account's kind, a forge
  // error kept on the record): refetch, then show the reason. onChanged clears
  // the banner synchronously and onError re-sets it in the same batch — keep
  // this order.
  const fail = (e: unknown) => {
    onChanged();
    onError(errorMessage(e));
  };
  const apply = async () => {
    // A known non-bot account needs the operator's word up front. An account
    // of UNKNOWN kind (a connection older than the field) is tried as-is: the
    // server learns the kind from the forge and only a 409 asks for the word.
    let force = false;
    if (conn.account_kind && !isBot) {
      if (!(await vouch())) return;
      force = true;
    }
    setBusy(true);
    try {
      await applyForgeConnectionAvatar(teamID, conn.id, { force });
      onChanged();
    } catch (e) {
      if (!force && e instanceof ApiError && e.status === 409) {
        if (!(await vouch())) {
          onChanged(); // the server recorded the learned kind
          return;
        }
        try {
          await applyForgeConnectionAvatar(teamID, conn.id, { force: true });
          onChanged();
        } catch (e2) {
          fail(e2);
        }
        return;
      }
      fail(e);
    } finally {
      setBusy(false);
    }
  };

  const applied = conn.avatar_applied_at;
  return (
    <div className="space-y-1">
      {conn.avatar_error && (
        // A failed re-apply does not undo an earlier success: the account
        // still wears the avatar it got, and the badge below says so.
        <InlineBanner tone={applied ? "warning" : "danger"} layout="inline">
          {applied ? "Last avatar upload failed" : "iterion-bot avatar not applied"}:{" "}
          {conn.avatar_error}
        </InlineBanner>
      )}
      <div className="flex flex-wrap items-center gap-2 text-caption text-fg-muted">
        {applied ? (
          <Badge variant="success" size="sm">
            iterion-bot avatar · {formatDate(applied)}
          </Badge>
        ) : (
          <span>{isBot ? "Bot account" : "Account"} without the iterion-bot avatar.</span>
        )}
        {canManage && (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => void apply()}
            loading={busy}
            disabled={busy}
            title="Upload the iterion-bot avatar onto this account"
          >
            {applied || conn.avatar_error ? "Re-apply the avatar" : "Apply iterion-bot avatar"}
          </Button>
        )}
      </div>
    </div>
  );
}

// RepoRow renders one enabled repo with its issue-sync toggle ("Sync issues
// to board", forge→board mirroring) and a "Sync now" ghost button that fires
// a one-shot sync and surfaces the returned counts inline. Local optimistic
// state on the toggle keeps the checkbox responsive across the PATCH.
function RepoRow({
  teamID,
  integration,
  canManage,
  onChanged,
  onDisable,
  onError,
}: {
  teamID: string;
  integration: ForgeIntegration;
  canManage: boolean;
  onChanged: () => void;
  onDisable: () => void;
  onError: (m: string) => void;
}) {
  const [syncEnabled, setSyncEnabled] = useState(!!integration.sync_issues_enabled);
  const [lastSync, setLastSync] = useState<ForgeSyncResult | null>(null);
  const toggleAction = useAsyncAction();
  const syncAction = useAsyncAction();

  const onToggle = async (next: boolean) => {
    // Optimistic: flip immediately, revert on error so the checkbox never
    // strands in a state the server didn't accept.
    setSyncEnabled(next);
    const updated = await toggleAction.run(() =>
      updateForgeIntegration(teamID, integration.id, { sync_issues_enabled: next }),
    );
    if (updated) {
      setSyncEnabled(!!updated.sync_issues_enabled);
      onChanged();
    } else {
      setSyncEnabled(!next);
      onError(toggleAction.error ?? "Failed to update sync setting");
    }
  };

  const onSyncNow = async () => {
    const res = await syncAction.run(() => syncForgeIntegration(teamID, integration.id));
    if (res) {
      setLastSync(res);
    } else if (syncAction.error) {
      onError(syncAction.error);
    }
  };

  return (
    <li className="border-t border-border-subtle pt-1 space-y-1">
      <div className="flex items-center justify-between gap-2 text-sm">
        <span>
          <Link
            href={repoDetailPath(integration)}
            className="font-mono hover:underline"
            title={`Repository details — ${integration.repo_full_name}`}
          >
            {integration.repo_full_name}
          </Link>{" "}
          <span className="text-fg-muted">· {integration.bot_ids.join(", ")}</span>
        </span>
        {canManage && (
          <span className="flex shrink-0 items-center gap-1.5">
            <Link
              href={bindBotPath({
                repoKey: forgeTeamRepoKey(integration),
                returnTo: "/integrations",
              })}
            >
              <Button variant="ghost" size="sm" title="Bind another bot to this repo">
                Bind bot…
              </Button>
            </Link>
            <Button variant="danger" size="sm" onClick={onDisable}>
              Disable
            </Button>
          </span>
        )}
      </div>
      {canManage && (
        <div className="flex items-center justify-between gap-2">
          <Checkbox
            label="Sync issues to board"
            checked={syncEnabled}
            disabled={toggleAction.busy}
            onChange={(e) => void onToggle(e.target.checked)}
          />
          <Button
            variant="ghost"
            size="sm"
            onClick={() => void onSyncNow()}
            loading={syncAction.busy}
            disabled={syncAction.busy}
            title="Pull the forge's issues onto the board now"
          >
            Sync now
          </Button>
        </div>
      )}
      {lastSync && (
        <InlineBanner tone="success" layout="inline">
          Synced {lastSync.synced} issue{lastSync.synced === 1 ? "" : "s"} ·{" "}
          {lastSync.created} created · {lastSync.updated} updated
        </InlineBanner>
      )}
      <WebhooksPanel teamID={teamID} integrationID={integration.id} />
    </li>
  );
}

// WebhooksPanel is a collapsible audit panel that lazy-loads the forge-side
// registered hooks for an integration's repo on first expand, so the operator
// can confirm iterion's webhook is still live on the forge.
function WebhooksPanel({
  teamID,
  integrationID,
}: {
  teamID: string;
  integrationID: string;
}) {
  const [expanded, setExpanded] = useState(false);
  const [hooks, setHooks] = useState<ForgeHook[] | null>(null);
  const action = useAsyncAction();

  const load = async () => {
    const res = await action.run(() => listForgeIntegrationHooks(teamID, integrationID));
    if (res) setHooks(res);
  };

  const toggle = () => {
    const next = !expanded;
    setExpanded(next);
    if (next && hooks === null && !action.busy) void load();
  };

  return (
    <div>
      <button
        type="button"
        onClick={toggle}
        className="text-micro text-accent-text hover:underline"
        aria-expanded={expanded}
      >
        {expanded ? "Hide webhooks" : "Webhooks"}
      </button>
      {expanded && (
        <div className="mt-1 space-y-1">
          {action.busy && (
            <p className="text-micro text-fg-subtle italic">Loading webhooks…</p>
          )}
          {action.error && (
            <InlineBanner tone="danger" layout="inline">
              {action.error}
            </InlineBanner>
          )}
          {hooks && hooks.length === 0 && !action.busy && !action.error && (
            <p className="text-micro text-fg-subtle">
              No webhooks registered on the forge for this repo.
            </p>
          )}
          {(hooks ?? []).map((h) => (
            <div
              key={h.id}
              className="rounded border border-border-subtle bg-surface-2 p-1.5 space-y-0.5"
            >
              <div className="flex items-center gap-2">
                <Badge variant={h.active ? "success" : "neutral"} size="sm">
                  {h.active ? "active" : "inactive"}
                </Badge>
                <span className="font-mono text-micro text-fg-default truncate" title={h.url}>
                  {h.url}
                </span>
              </div>
              {h.events.length > 0 && (
                <div className="text-micro text-fg-muted truncate">
                  {h.events.join(", ")}
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
