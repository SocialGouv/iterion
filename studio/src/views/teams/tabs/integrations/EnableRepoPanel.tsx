import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import {
  type ForgeConnection,
  type ForgeRepo,
  enableForgeRepoBots,
  getForgeConnectionHealth,
  listForgeRepos,
  previewForgeEnable,
} from "@/api/forgeConnections";
import CronField from "@/components/shared/CronField";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import {
  GROUP_LABELS,
  GROUP_ORDER,
  hasSchedule,
  primaryGroup,
  scheduleCronFor,
  triggerChips,
} from "@/lib/triggers";

export function EnableRepoPanel({
  teamID,
  conn,
  repoBots,
  preselectBot,
  onDone,
  onCancel,
  onError,
}: {
  teamID: string;
  conn: ForgeConnection;
  repoBots: BotEntryWithSchema[];
  preselectBot?: string;
  /** Called once the server accepts the enable request. The optional
   *  argument surfaces the repo that was just enabled so callers (e.g.
   *  the connect wizard) can jump straight to it — legacy callers may
   *  ignore it (backward-compatible with the old no-arg signature). */
  onDone: (enabled?: { repo: string; connectionID: string }) => void;
  onCancel: () => void;
  onError: (m: string) => void;
}) {
  const [search, setSearch] = useState("");
  const [repos, setRepos] = useState<ForgeRepo[]>([]);
  const [loadingRepos, setLoadingRepos] = useState(false);
  const [repo, setRepo] = useState("");
  const [selectedBots, setSelectedBots] = useState<string[]>(
    preselectBot ? [preselectBot] : [],
  );
  // Per-bot cron overrides for scheduled bots (bot name → cron); empty entries
  // fall back to the manifest suggested_cron server-side.
  const [scheduleCrons, setScheduleCrons] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);

  const loadRepos = async () => {
    setLoadingRepos(true);
    try {
      setRepos(await listForgeRepos(teamID, conn.id, search));
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setLoadingRepos(false);
    }
  };

  useEffect(() => {
    void loadRepos();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // GitHub App installations only expose the repos the operator granted
  // on GitHub — an empty search here used to dead-end with no
  // explanation. The health probe surfaces the installation's live
  // scope + the GitHub settings URL where it can be widened. Probe
  // failures deliberately just hide the banner.
  const healthQuery = useQuery({
    queryKey: ["forge-connection-health", teamID, conn.id],
    queryFn: () => getForgeConnectionHealth(teamID, conn.id),
    enabled: conn.kind === "github_app",
  });
  const health = healthQuery.data ?? null;

  // Fetch the authoritative preview (native events the hook will subscribe
  // to + identity + any scope/forge-block conflicts) whenever the selection
  // changes, so the operator sees exactly what Enable will provision.
  // Failures deliberately collapse to "no preview box".
  const previewEnabled = repo !== "" && selectedBots.length > 0;
  const previewQuery = useQuery({
    queryKey: ["forge-enable-preview", teamID, conn.id, repo, selectedBots],
    queryFn: () => previewForgeEnable(teamID, conn.id, repo, selectedBots),
    enabled: previewEnabled,
    // Keep the previous selection's preview on screen while the next one
    // loads, instead of blinking the box out on every checkbox toggle.
    placeholderData: keepPreviousData,
  });
  const preview = previewEnabled ? previewQuery.data ?? null : null;

  const toggleBot = (name: string) =>
    setSelectedBots((s) => (s.includes(name) ? s.filter((b) => b !== name) : [...s, name]));

  const hasConflicts = (preview?.conflicts?.length ?? 0) > 0;

  const enable = async () => {
    if (!repo || selectedBots.length === 0) return;
    setBusy(true);
    try {
      // Collect the cron for each selected scheduled bot (operator override or
      // the suggested default), so the server provisions the chosen cadence.
      const crons: Record<string, string> = {};
      for (const b of repoBots) {
        if (selectedBots.includes(b.name) && hasSchedule(b)) {
          const cron = (scheduleCrons[b.name] ?? scheduleCronFor(b)).trim();
          if (cron) crons[b.name] = cron;
        }
      }
      await enableForgeRepoBots(teamID, conn.id, repo, selectedBots, crons);
      onDone({ repo, connectionID: conn.id });
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="bg-surface-0 border border-border-subtle rounded p-3 space-y-3">
      <div className="flex gap-2 items-center">
        <div className="flex-1">
          <label htmlFor="forge-repo-search" className="sr-only">
            Search repos
          </label>
          <Input
            id="forge-repo-search"
            placeholder="Search repos…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void loadRepos();
            }}
          />
        </div>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => void loadRepos()}
          loading={loadingRepos}
        >
          {loadingRepos ? "…" : "Search"}
        </Button>
      </div>

      {conn.kind === "github_app" && health && (
        <div
          className={`rounded border px-2.5 py-2 text-xs ${
            !loadingRepos && repos.length === 0
              ? "border-warning/40 bg-warning-soft text-warning-fg"
              : "border-border-subtle bg-surface-1 text-fg-muted"
          }`}
        >
          The GitHub App installation
          {health.installation_account ? ` on ${health.installation_account}` : ""} covers{" "}
          {health.installation_repos?.length ?? 0} repositor
          {(health.installation_repos?.length ?? 0) === 1 ? "y" : "ies"}. A repo missing
          here must first be granted to the installation on GitHub.
          {health.manage_install_url && (
            <>
              {" "}
              <a
                href={health.manage_install_url}
                target="_blank"
                rel="noreferrer"
                className="text-accent-text underline"
              >
                Add repositories on GitHub ↗
              </a>{" "}
              then hit Search again.
            </>
          )}
        </div>
      )}

      <div>
        <label htmlFor="forge-repo-pick" className="sr-only">
          Repository
        </label>
        <Select
          id="forge-repo-pick"
          value={repo}
          onChange={(e) => setRepo(e.target.value)}
        >
          <option value="">Select a repository…</option>
          {repos.map((r) => (
            <option key={r.full_name} value={r.full_name} disabled={!r.can_admin}>
              {r.full_name}
              {r.can_admin ? "" : " (no admin access)"}
            </option>
          ))}
        </Select>
      </div>

      <div>
        <div className="text-xs uppercase tracking-wider text-fg-muted mb-1">Bots to enable</div>
        {repoBots.length === 0 ? (
          <div className="text-fg-muted text-sm">
            No repo-installable bots found (a bot needs an{" "}
            <span className="font-mono">invocations:</span> block in its manifest).
          </div>
        ) : (
          <div className="space-y-3">
            {GROUP_ORDER.map((group) => {
              const inGroup = repoBots.filter((b) => primaryGroup(b) === group);
              if (inGroup.length === 0) return null;
              return (
                <div key={group}>
                  <div className="text-caption text-fg-muted mb-1">{GROUP_LABELS[group]}</div>
                  <ul className="space-y-2">
                    {inGroup.map((b) => (
                      <li key={b.name} className="space-y-1">
                        <div className="flex gap-2">
                          <Checkbox
                            id={`fb-${b.name}`}
                            checked={selectedBots.includes(b.name)}
                            onChange={() => toggleBot(b.name)}
                            className="mt-1"
                          />
                          <label htmlFor={`fb-${b.name}`} className="text-sm">
                            <span className="font-medium">{b.display_name || b.name}</span>{" "}
                            <span className="font-mono text-fg-muted">{b.name}</span>
                            <span className="mt-0.5 flex flex-wrap gap-1">
                              {triggerChips(b).map((c) => (
                                <span
                                  key={c}
                                  className="inline-block font-mono text-caption text-fg-muted bg-surface-1 border border-border-subtle rounded px-1"
                                >
                                  {c}
                                </span>
                              ))}
                            </span>
                          </label>
                        </div>
                        {selectedBots.includes(b.name) && hasSchedule(b) && (
                          <div className="ml-6 max-w-sm">
                            <span className="text-caption text-fg-muted">
                              cron (UTC — or prefix CRON_TZ=&lt;zone&gt;)
                            </span>
                            <CronField
                              value={scheduleCrons[b.name] ?? scheduleCronFor(b)}
                              onChange={(v) =>
                                setScheduleCrons((s) => ({ ...s, [b.name]: v }))
                              }
                              disabled={busy}
                              hideLabel
                              ariaLabel={`Cron schedule for ${b.display_name || b.name} (5-field, UTC — or prefix CRON_TZ=<zone>)`}
                            />
                          </div>
                        )}
                      </li>
                    ))}
                  </ul>
                </div>
              );
            })}
          </div>
        )}
      </div>

      {preview && (
        <div className="bg-surface-1 border border-border-subtle rounded p-2 text-xs space-y-1">
          {preview.forge_native_events.length > 0 && (
            <div>
              <span className="text-fg-muted">Will subscribe to:</span>{" "}
              <span className="font-mono">{preview.forge_native_events.join(", ")}</span>
            </div>
          )}
          {(preview.commands?.length ?? 0) > 0 && (
            <div>
              <span className="text-fg-muted">Commands:</span>{" "}
              {preview.commands?.map((c) => (
                <span key={c.command} className="font-mono mr-1">
                  /{c.command}
                </span>
              ))}
            </div>
          )}
          {preview.identity.handle && (
            <div>
              <span className="text-fg-muted">Will post as:</span> @{preview.identity.handle}
            </div>
          )}
          {hasConflicts &&
            preview.conflicts.map((c) => (
              <div key={c} className="text-danger">
                ⚠ {c}
              </div>
            ))}
        </div>
      )}

      <div className="flex items-center gap-2">
        <Button
          variant="primary"
          onClick={() => void enable()}
          disabled={busy || !repo || selectedBots.length === 0 || hasConflicts}
          loading={busy}
        >
          {busy ? "Enabling…" : "Enable"}
        </Button>
        <Button variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </div>
  );
}
