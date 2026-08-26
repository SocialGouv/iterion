// "Target repository" section of the Launch form.
//
// A bot declares its runtime repo need with a manifest `repo:` block
// (mode: required | optional | none, allow_create, purpose, visibility).
// This section turns that into an attach-or-create choice for the operator:
//
//   - Use <active repo>          (default when an active repo exists)
//   - Another connected repo     (Select over the team's connected repos)
//   - Create a new repository    (opt-in, when repo.allow_create)
//   - No repository              (only when repo.mode === "optional")
//
// State (RepoTargetState) is owned by LaunchView — this component is
// presentational + validation helpers. LaunchView calls createForgeRepo()
// on submit when mode === "create", then feeds the returned clone_url +
// connection_id into createRun() as repo_url / connection_id.

import { useEffect, useMemo } from "react";
import { useLocation } from "wouter";
import { useQuery } from "@tanstack/react-query";

import type { RepoRequirement } from "@/api/bots";
import {
  forgeTeamRepoKey,
  listForgeConnections,
  type ForgeConnection,
  type ForgeTeamRepo,
} from "@/api/forgeConnections";

import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Radio } from "@/components/ui/Radio";
import { Select } from "@/components/ui/Select";

export type RepoChoiceMode = "active" | "attach" | "create" | "none";

export interface RepoTargetState {
  mode: RepoChoiceMode;
  /** Selected repo key (forgeTeamRepoKey) in "attach" mode. */
  attachKey: string;
  /** Selected forge connection id in "create" mode. */
  createConnectionID: string;
  /** Owner/namespace override in "create" mode; empty = the account itself. */
  createOwner: string;
  /** Repository name in "create" mode. */
  createName: string;
  /** Visibility toggle in "create" mode (true = private). */
  createPrivate: boolean;
}

export interface RepoTargetSectionProps {
  /** The bot's manifest `repo:` block. Parent gates rendering on non-"none". */
  repo: RepoRequirement;
  activeRepo: ForgeTeamRepo | null;
  repos: ForgeTeamRepo[];
  teamID: string | null;
  state: RepoTargetState;
  onChange: (patch: Partial<RepoTargetState>) => void;
  /** Error surfaced from createForgeRepo (409 name taken / 422 App perms /
   *  network); shown inline in the create sub-form. */
  createError: string | null;
  submitting: boolean;
  /** The launch form's ?file= path — round-tripped through the connect
   *  wizard's returnTo so the operator lands back on this exact launch
   *  screen once a forge is wired. */
  filePath: string;
}

/** Derive an initial RepoTargetState from the bot's requirement + the
 *  team's connected repos. Prefers the strongest available option:
 *  active > attach > create > none. */
export function initialRepoTargetState(
  repo: RepoRequirement,
  activeRepo: ForgeTeamRepo | null,
  repos: ForgeTeamRepo[],
): RepoTargetState {
  const canActive = !!activeRepo;
  const canAttach = repos.length > 0;
  const canCreate = !!repo.allow_create;
  const canNone = repo.mode === "optional";
  let mode: RepoChoiceMode;
  if (canActive) mode = "active";
  else if (canAttach) mode = "attach";
  else if (canCreate) mode = "create";
  else if (canNone) mode = "none";
  else mode = repo.mode === "optional" ? "none" : "create";
  return {
    mode,
    attachKey: "",
    createConnectionID: "",
    createOwner: "",
    createName: "",
    createPrivate: repo.visibility !== "public",
  };
}

/** True when the current state resolves to a launchable target. `none`
 *  is only valid when the bot's repo mode is "optional". */
export function isRepoTargetValid(
  repo: RepoRequirement,
  state: RepoTargetState,
  activeRepo: ForgeTeamRepo | null,
  repos: ForgeTeamRepo[],
): boolean {
  switch (state.mode) {
    case "active":
      return !!activeRepo;
    case "attach":
      return (
        !!state.attachKey && repos.some((r) => forgeTeamRepoKey(r) === state.attachKey)
      );
    case "create":
      return !!(state.createConnectionID && state.createName.trim());
    case "none":
      return repo.mode === "optional";
  }
}

/** Look up a repo row by its stable key. */
export function findAttachedRepo(
  repos: ForgeTeamRepo[],
  key: string,
): ForgeTeamRepo | null {
  return repos.find((r) => forgeTeamRepoKey(r) === key) ?? null;
}

// Stable empty fallback so the query's undefined→loaded transition doesn't
// hand consumers a fresh [] reference each render (same trick as
// useActiveRepo).
const EMPTY_CONNECTIONS: ForgeConnection[] = [];

function ownerOf(fullName: string): string {
  const [owner] = fullName.split("/");
  return owner ?? "";
}

function connectionLabel(c: ForgeConnection): string {
  if (c.display_name) return c.display_name;
  const who = c.namespace || c.account_login || "";
  return who ? `${c.provider} · ${who}` : c.provider;
}

export default function RepoTargetSection({
  repo,
  activeRepo,
  repos,
  teamID,
  state,
  onChange,
  createError,
  submitting,
  filePath,
}: RepoTargetSectionProps) {
  const [, setLocation] = useLocation();
  // Connections list: fetched whenever create mode is open — a freshly
  // added connection has no provisioned repo yet, so deriving options
  // from `repos` alone would hide it (seen live: a new PAT connection
  // was unselectable for create). Repo-derived entries stay first for
  // familiar labels; the full list unions in behind them.
  const connectionsQuery = useQuery<ForgeConnection[]>({
    queryKey: ["forge-connections", teamID],
    queryFn: () => listForgeConnections(teamID ?? ""),
    enabled: state.mode === "create" && !!teamID,
    staleTime: 30_000,
  });
  // A watch-only connection (Dependabot alerts, read) cannot clone or push, so
  // it is never a launch target. The server refuses it with a 422; not
  // offering it is what keeps the operator from meeting that refusal at all.
  const connections: ForgeConnection[] =
    connectionsQuery.data?.filter((c) => c.purpose !== "security_read") ??
    EMPTY_CONNECTIONS;
  const connectionsLoading = connectionsQuery.isLoading && repos.length === 0;

  const createConnOptions = useMemo(() => {
    const out: Array<{ id: string; label: string }> = [];
    const seen = new Set<string>();
    for (const r of repos) {
      if (seen.has(r.connection_id)) continue;
      seen.add(r.connection_id);
      out.push({ id: r.connection_id, label: `${r.provider} · ${ownerOf(r.repo_full_name)}` });
    }
    for (const c of connections) {
      if (seen.has(c.id)) continue;
      seen.add(c.id);
      out.push({ id: c.id, label: connectionLabel(c) });
    }
    return out;
  }, [repos, connections]);

  // Owner default when the user picks a connection: pre-fill from the
  // first repo on that connection (its "owner" segment) so a fresh
  // "create" form isn't blank when it doesn't need to be. Only touched
  // when the user hasn't already typed an owner.
  useEffect(() => {
    if (state.mode !== "create") return;
    if (!state.createConnectionID) return;
    if (state.createOwner) return;
    const sample = repos.find((r) => r.connection_id === state.createConnectionID);
    if (sample) {
      onChange({ createOwner: ownerOf(sample.repo_full_name) });
      return;
    }
    const conn = connections.find((c) => c.id === state.createConnectionID);
    if (conn) {
      const guess = conn.namespace || conn.account_login || "";
      if (guess) onChange({ createOwner: guess });
    }
  }, [
    state.mode,
    state.createConnectionID,
    state.createOwner,
    repos,
    connections,
    onChange,
  ]);

  const canActive = !!activeRepo;
  const canAttach = repos.length > 0;
  const canCreate = !!repo.allow_create;
  const canNone = repo.mode === "optional";
  const noOptions = !canActive && !canAttach && !canCreate;

  return (
    <section className="mt-6 border-t border-border-default pt-4 mb-6">
      <h2 className="text-xs font-medium text-fg-muted mb-1">Target repository</h2>
      {repo.purpose && (
        <p className="text-caption text-fg-subtle mb-3">{repo.purpose}</p>
      )}
      <div
        className="space-y-3"
        role="radiogroup"
        aria-label="Target repository"
      >
        {canActive && activeRepo && (
          <div>
            <Radio
              name="repo-target"
              value="active"
              checked={state.mode === "active"}
              onChange={() => onChange({ mode: "active" })}
              disabled={submitting}
              label={
                <span>
                  Use{" "}
                  <span className="font-mono text-fg-default">
                    {activeRepo.repo_full_name}
                  </span>
                </span>
              }
            />
          </div>
        )}
        {canAttach && (
          <div>
            <Radio
              name="repo-target"
              value="attach"
              checked={state.mode === "attach"}
              onChange={() => onChange({ mode: "attach" })}
              disabled={submitting}
              label={<span>Another connected repo</span>}
            />
            {state.mode === "attach" && (
              <div className="mt-2 pl-6">
                <Select
                  id="repo-target-attach"
                  value={state.attachKey}
                  onChange={(e) => onChange({ attachKey: e.target.value })}
                  disabled={submitting}
                >
                  <option value="">Select a repository…</option>
                  {repos.map((r) => {
                    const key = forgeTeamRepoKey(r);
                    return (
                      <option key={key} value={key}>
                        {r.provider} · {r.repo_full_name}
                      </option>
                    );
                  })}
                </Select>
              </div>
            )}
          </div>
        )}
        {canCreate && (
          <div>
            <Radio
              name="repo-target"
              value="create"
              checked={state.mode === "create"}
              onChange={() => onChange({ mode: "create" })}
              disabled={submitting}
              label={<span>Create a new repository</span>}
            />
            {state.mode === "create" && (
              <div className="mt-2 pl-6 space-y-3">
                <div className="grid grid-cols-[100px_1fr] gap-2 items-start">
                  <label
                    htmlFor="repo-target-conn"
                    className="pt-1 text-xs text-fg-subtle"
                  >
                    Connection
                  </label>
                  <Select
                    id="repo-target-conn"
                    value={state.createConnectionID}
                    onChange={(e) =>
                      onChange({ createConnectionID: e.target.value, createOwner: "" })
                    }
                    disabled={submitting || connectionsLoading}
                  >
                    <option value="">
                      {connectionsLoading
                        ? "Fetching connections…"
                        : "Select a connection…"}
                    </option>
                    {createConnOptions.map((c) => (
                      <option key={c.id} value={c.id}>
                        {c.label}
                      </option>
                    ))}
                  </Select>
                </div>
                <div className="grid grid-cols-[100px_1fr] gap-2 items-start">
                  <label
                    htmlFor="repo-target-owner"
                    className="pt-1 text-xs text-fg-subtle"
                  >
                    Owner
                  </label>
                  <div>
                    <Input
                      id="repo-target-owner"
                      type="text"
                      value={state.createOwner}
                      onChange={(e) => onChange({ createOwner: e.target.value })}
                      disabled={submitting}
                      placeholder="org / namespace"
                    />
                    <div className="mt-1 text-caption text-fg-subtle">
                      org / namespace; empty = the account itself
                    </div>
                  </div>
                </div>
                <div className="grid grid-cols-[100px_1fr] gap-2 items-start">
                  <label
                    htmlFor="repo-target-name"
                    className="pt-1 text-xs text-fg-subtle"
                  >
                    Name
                  </label>
                  <Input
                    id="repo-target-name"
                    type="text"
                    required
                    value={state.createName}
                    onChange={(e) => onChange({ createName: e.target.value })}
                    disabled={submitting}
                    placeholder="my-new-app"
                  />
                </div>
                <div className="grid grid-cols-[100px_1fr] gap-2 items-start">
                  <label
                    htmlFor="repo-target-vis"
                    className="pt-1 text-xs text-fg-subtle"
                  >
                    Visibility
                  </label>
                  <Select
                    id="repo-target-vis"
                    value={state.createPrivate ? "private" : "public"}
                    onChange={(e) =>
                      onChange({ createPrivate: e.target.value === "private" })
                    }
                    disabled={submitting}
                  >
                    <option value="private">Private</option>
                    <option value="public">Public</option>
                  </Select>
                </div>
                <p className="text-caption text-fg-subtle">
                  The repository is created on the forge at launch.
                </p>
                {createError && (
                  <InlineBanner tone="danger" layout="inline">
                    {createError}
                  </InlineBanner>
                )}
              </div>
            )}
          </div>
        )}
        {canNone && (
          <div>
            <Radio
              name="repo-target"
              value="none"
              checked={state.mode === "none"}
              onChange={() => onChange({ mode: "none" })}
              disabled={submitting}
              label={<span>No repository (local workspace)</span>}
            />
          </div>
        )}
        {noOptions && (
          <EmptyState
            title={
              repo.mode === "required"
                ? "This bot needs a target repository"
                : "No connected repository yet"
            }
            message={
              repo.mode === "required"
                ? "Connect a forge (GitHub, GitLab, Forgejo) so the run has somewhere to clone into. Come back to this launch form when the wizard finishes."
                : "Connect a forge to attach or create a repository. You can also skip and launch without a repo — this bot supports that."
            }
            action={
              <Button
                variant="primary"
                size="sm"
                onClick={() => {
                  const back = filePath
                    ? `/runs/new?file=${encodeURIComponent(filePath)}`
                    : "/runs/new";
                  setLocation(
                    `/integrations/connect?returnTo=${encodeURIComponent(back)}`,
                  );
                }}
              >
                Connect a repository
              </Button>
            }
          />
        )}
      </div>
    </section>
  );
}
