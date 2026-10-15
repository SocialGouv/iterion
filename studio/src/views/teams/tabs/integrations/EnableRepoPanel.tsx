import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { Link } from "wouter";

import type { BotEntryWithSchema } from "@/api/bots";
import {
  type ForgeConnection,
  type ForgeConnectionHealth,
  type ForgeRepo,
  enableForgeRepoBots,
  getForgeConnectionHealth,
  listForgeIntegrations,
  listForgeRepos,
  previewForgeEnable,
} from "@/api/forgeConnections";
import CronField from "@/components/shared/CronField";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Input } from "@/components/ui/Input";
import { Radio } from "@/components/ui/Radio";
import { Select } from "@/components/ui/Select";
import { hookBaseProblem, hookDeliveryAddress } from "@/lib/hookBase";
import { mergeGateLabel } from "@/lib/mergeGate";
import { repoDetailPath } from "@/views/RepoDetail/repoKey";
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
  onDone: (enabled?: { repo: string; connectionID: string; pending?: boolean }) => void;
  onCancel: () => void;
  /** Reports into the parent's error slot. The EMPTY STRING is a message of
   *  this channel too — "an attempt started, take the banner down" — so a
   *  parent must read it with `||`, never `??`. Both parents do. */
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
  // The merge-gate posture for a FRESH provision: "Gate merges" (the server's
  // default — nothing pinned) or "Advisory only" (gate_enabled: "false").
  const [gateOn, setGateOn] = useState(true);
  const [busy, setBusy] = useState(false);

  // The posture belongs to the repo it was picked for: switching the
  // Repository select starts the next one on the server's default, which is
  // what the radios then show.
  useEffect(() => {
    setGateOn(true);
  }, [repo]);

  // The team's integrations, ALWAYS read here, under the Integrations tab's
  // own cache key: whether a list is fresh is a fact about the read, and only
  // its owner holds it. A list handed down as a prop is a snapshot whose
  // freshness the panel cannot check — and this cache key keeps a successful
  // snapshot next to the error of a refetch that failed, so a present list is
  // not a current one. Until the posture can be read from a settled answer,
  // none is offered: the panel could not tell a fresh repo from an integrated
  // one, and a list predating this repo's integration would offer a posture
  // the repo already pins.
  const integrationsQuery = useQuery({
    queryKey: ["forge-integrations", teamID],
    queryFn: () => listForgeIntegrations(teamID),
  });
  // Settled, hence a fact: a cache entry in error, or one being refetched, is
  // unknown however much data sits beside it.
  const integrationsReady = integrationsQuery.isSuccess && !integrationsQuery.isFetching;
  const knownIntegrations = integrationsQuery.data;
  const integrationsError = integrationsQuery.isError
    ? errorMessage(integrationsQuery.error)
    : null;

  // The integration this repo already has on this connection. Its posture is
  // pinned on launch_vars and changed on the repo page: a re-enable must not
  // restate those pins (a non-nil launch_vars is the exact desired set
  // server-side, and a partial one that would drop a pin is refused).
  const existing = useMemo(
    () =>
      (knownIntegrations ?? []).find(
        (i) => i.connection_id === conn.id && i.repo_full_name === repo,
      ) ?? null,
    [knownIntegrations, conn.id, repo],
  );

  // Loading the repo list reports into the tab's shared banner and, like the
  // row's "Sync now", never reaches onChanged — so nothing else takes that
  // banner down. The attempt clears it: otherwise a list that failed once
  // keeps claiming it while the repos a later search loaded are on screen.
  const loadRepos = async () => {
    onError("");
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
  // The connection's pinned webhook base is part of the key: it decides the
  // hook_base_url the preview reports, so a pin changed from the row above
  // must re-query rather than leave the old address on screen.
  const previewEnabled = repo !== "" && selectedBots.length > 0;
  const previewQuery = useQuery({
    queryKey: [
      "forge-enable-preview",
      teamID,
      conn.id,
      repo,
      selectedBots,
      conn.webhook_base_url ?? "",
    ],
    queryFn: () => previewForgeEnable(teamID, conn.id, repo, selectedBots),
    enabled: previewEnabled,
    // Keep the previous selection's preview on screen while the next one
    // loads, instead of blinking the box out on every checkbox toggle.
    placeholderData: keepPreviousData,
  });
  const preview = previewEnabled ? previewQuery.data ?? null : null;
  // Only a SETTLED answer for THIS selection describes it: a kept one belongs
  // to the previous selection (or the previous pin), and before the first one
  // lands there is no answer at all. The delivery address, the posture and
  // Enable itself therefore read a fresh answer only — the box's other lines
  // may keep the old one rather than blink out.
  const previewFresh =
    previewEnabled && previewQuery.isSuccess && !previewQuery.isPlaceholderData;
  const freshPreview = previewFresh ? preview : null;
  // A preview that failed is why Enable is out and why the box lost its
  // lines: said here, where the box is, rather than nowhere.
  const previewError =
    previewEnabled && previewQuery.isError ? errorMessage(previewQuery.error) : null;
  // What the operator must be told about the announced base, from the one
  // table both provisioning surfaces read. A server that reported no base at
  // all gave no answer about the address: nothing is said about it, where an
  // empty base IS an answer ("no public URL") and is warned about.
  const announcedBase = freshPreview?.hook_base_url;
  const hookBaseIssue =
    announcedBase === undefined ? null : hookBaseProblem(announcedBase);

  const toggleBot = (name: string) =>
    setSelectedBots((s) => (s.includes(name) ? s.filter((b) => b !== name) : [...s, name]));

  const hasConflicts = (preview?.conflicts?.length ?? 0) > 0;

  // The posture is a choice only for a repo integrated here for the first
  // time, once the integrations are known, and while a selected bot gates
  // merges. The choice travels only while it is on screen: an "Advisory only"
  // picked before the gating bot was unticked is not sent behind the
  // operator's back.
  const offersPosture =
    repo !== "" && integrationsReady && !existing && !!freshPreview?.gates_merges;
  const showsPosture =
    repo !== "" &&
    integrationsReady &&
    existing !== null &&
    (!!freshPreview?.gates_merges || existing.launch_vars?.gate_enabled !== undefined);
  // While a selected bot gates merges and the integrations are being read —
  // the first read, or a re-read when the window regains focus — the posture
  // cannot be decided, so Enable waits: a click then would send the default
  // posture in place of the one the operator is choosing.
  const postureUndecided =
    repo !== "" && !!freshPreview?.gates_merges && !integrationsReady && !integrationsError;

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
      // Only a repo being integrated here for the first time carries a
      // posture pin: it has no stored launch_vars to drop, and the default
      // posture pins nothing at all.
      const launchVars = offersPosture && !gateOn ? { gate_enabled: "false" } : undefined;
      const res = await enableForgeRepoBots(
        teamID,
        conn.id,
        repo,
        selectedBots,
        crons,
        launchVars,
      );
      onDone({ repo, connectionID: conn.id, pending: !!res.pending_approval });
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

      {conn.kind === "github_app" && health && <MissingGrantLines health={health} />}

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

      {(preview || previewError) && (
        <div className="bg-surface-1 border border-border-subtle rounded p-2 text-xs space-y-1">
          {previewError && (
            <div className="text-warning-fg">
              What this selection would provision could not be read ({previewError}) — Enable
              stays out until it can.
            </div>
          )}
          {preview && (
            <>
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
              {announcedBase !== undefined && (
                <div>
                  <span className="text-fg-muted">Will deliver to:</span>{" "}
                  {hookBaseIssue === null ? (
                    <span className="font-mono">
                      {hookDeliveryAddress(announcedBase, conn.provider)}
                    </span>
                  ) : (
                    // The base is empty when the deployment has no public URL,
                    // and not an absolute http(s) URL when what it has is not
                    // an address: either way the hook URL is one every forge
                    // refuses, so no address is announced.
                    <span className="text-warning-fg">
                      {hookBaseIssue}; pin a base URL on the connection above.
                    </span>
                  )}
                </div>
              )}
              {hasConflicts &&
                preview.conflicts.map((c) => (
                  <div key={c} className="text-danger">
                    ⚠ {c}
                  </div>
                ))}
            </>
          )}
        </div>
      )}

      {showsPosture && existing && (
        // The posture of a repo already integrated here is pinned on its
        // integration and changed over that whole map — on the repo page,
        // which owns the write. Said here so a re-enable does not read as a
        // chance to change it.
        <div className="text-xs text-fg-muted">
          Merge gate: {mergeGateLabel(existing.launch_vars)} —{" "}
          <Link href={repoDetailPath(existing)} className="text-accent-text underline">
            change it on the repo page
          </Link>
        </div>
      )}

      {postureUndecided && (
        <div className="text-xs text-fg-muted">
          Merge gate: reading this team&apos;s enabled repos…
        </div>
      )}

      {repo !== "" && integrationsError && freshPreview?.gates_merges && (
        <div className="text-xs text-warning-fg">
          Merge gate: this team&apos;s enabled repos could not be read ({integrationsError}), so
          no posture is offered — Enable keeps the stored one, or the default for a new repo.
        </div>
      )}

      {offersPosture && (
        <fieldset className="space-y-1.5">
          <legend className="text-xs uppercase tracking-wider text-fg-muted">
            Merge gate
          </legend>
          <Radio
            name={`merge-gate-${conn.id}`}
            checked={gateOn}
            onChange={() => setGateOn(true)}
            disabled={busy}
            className="items-start"
            label={
              <span className="block">
                <span className="block text-sm font-medium text-fg-default">Gate merges</span>
                <span className="block text-caption text-fg-muted">
                  The bots post a merge-gate status and re-review every push to keep it
                  current.
                </span>
              </span>
            }
          />
          <Radio
            name={`merge-gate-${conn.id}`}
            checked={!gateOn}
            onChange={() => setGateOn(false)}
            disabled={busy}
            className="items-start"
            label={
              <span className="block">
                <span className="block text-sm font-medium text-fg-default">Advisory only</span>
                <span className="block text-caption text-fg-muted">
                  Reviews when the request opens and on demand, without a merge-gate status.
                </span>
              </span>
            }
          />
        </fieldset>
      )}

      <div className="flex items-center gap-2">
        <Button
          variant="primary"
          onClick={() => void enable()}
          // Without a fresh preview the panel names no address and offers no
          // posture: provisioning from that state would register an address,
          // and pin a posture, the operator never saw.
          disabled={
            busy ||
            !repo ||
            selectedBots.length === 0 ||
            hasConflicts ||
            !previewFresh ||
            postureUndecided
          }
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

// MissingGrantLines names each permission gap the health probe found, with
// what it costs and where it is approved — the same lines
// `iterion remote forge connections refresh` prints. Without them the API and
// the CLI were the only surfaces of a gap an operator meets as a dead panel
// or a run that fails hours in, at push time.
function MissingGrantLines({ health }: { health: ForgeConnectionHealth }) {
  const install = health.manage_install_url;
  const gaps: { key: string; text: string }[] = [];
  if (health.missing_permissions?.length) {
    gaps.push({
      key: "delivery",
      text: `Missing for app delivery (publishing the CI workflow and the image it builds): ${health.missing_permissions.join(", ")}.`,
    });
  }
  if (health.missing_ci_permissions?.length) {
    // Name every surface the gap darkens: `statuses` is also what the
    // revi/review merge-gate verdict is posted and read with, so an operator
    // told only about a card panel would not know the gate is dark too.
    const surfaces = health.missing_ci_permissions.includes("statuses")
      ? "the board card CI panel and the merge-gate verdict"
      : "the board card CI panel";
    gaps.push({
      key: "ci",
      text: `Missing for ${surfaces}: ${health.missing_ci_permissions.join(", ")}.`,
    });
  }
  if (health.missing_security_permissions?.length) {
    gaps.push({
      key: "security",
      text: `Missing for the org-wide Dependabot alerts read: ${health.missing_security_permissions.join(", ")}.`,
    });
  }
  if (gaps.length === 0) return null;
  return (
    <div className="rounded border border-warning/40 bg-warning-soft text-warning-fg px-2.5 py-2 text-xs space-y-1">
      {gaps.map((g) => (
        <div key={g.key}>{g.text}</div>
      ))}
      <div>
        An org owner approves the pending request{" "}
        {install ? (
          <a href={install} target="_blank" rel="noreferrer" className="text-accent-text underline">
            on the App&rsquo;s installation page ↗
          </a>
        ) : (
          "on the App’s installation page"
        )}
        .
      </div>
    </div>
  );
}
