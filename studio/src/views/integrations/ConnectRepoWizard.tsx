import { errorMessage } from "@/lib/errorHints";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useSearch } from "wouter";
import { CheckIcon } from "@radix-ui/react-icons";
import { useQueryClient } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import {
  type ConnectForgeInput,
  type ForgeConnection,
  type ForgeOAuthApp,
  type ForgeProvider,
  connectForge,
  forgeTeamRepoKey,
  getMyGitHubOrgs,
  listForgeConnections,
  listForgeOAuthApps,
  startGitHubManifest,
  startGitHubOrgsPicker,
} from "@/api/forgeConnections";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Textarea } from "@/components/ui/Textarea";
import { useAuth } from "@/auth/AuthContext";
import { useActiveRepoStore } from "@/store/activeRepo";
import { useBotsStore } from "@/store/bots";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useServerInfoStore } from "@/store/serverInfo";
import { isRepoCapable } from "@/lib/triggers";

import { EnableRepoPanel } from "@/views/teams/tabs/integrations/EnableRepoPanel";
import {
  CONNECTABLE,
  DEFAULT_BASE,
  canonicalBase,
} from "@/views/teams/tabs/integrations/forgeShared";
import { StepIndicator } from "@/views/integrations/wizard/StepIndicator";

// The wizard is the centrepiece of the cloud connect-repo UX redesign:
// four short steps (Provider → Authorize → Repositories → Done), driven
// straight off the URL query so a back-nav or an OAuth/App-install
// round-trip resumes exactly where it left off. Everything the wizard
// needs already lives in @/api/forgeConnections + EnableRepoPanel; this
// component only orchestrates them behind a single guided flow.

type Step = "provider" | "authorize" | "repos" | "done";
const STEP_ORDER: Step[] = ["provider", "authorize", "repos", "done"];
const STEP_LABEL: Record<Step, string> = {
  provider: "Provider",
  authorize: "Authorize",
  repos: "Repositories",
  done: "Done",
};

const RETURN_PATH = "/integrations/connect";

// Provider-facing metadata for the picker cards. GitHub is flagged as
// "Recommended" (least-privilege App path when the platform is wired).
const PROVIDER_META: Array<{
  id: ForgeProvider;
  title: string;
  blurb: string;
  recommended?: boolean;
}> = [
  {
    id: "github",
    title: "GitHub",
    blurb: "GitHub App (least privilege) or OAuth / PAT.",
    recommended: true,
  },
  {
    id: "gitlab",
    title: "GitLab",
    blurb: "OAuth if an app is registered — token otherwise.",
  },
  {
    id: "forgejo",
    title: "Forgejo",
    blurb: "Forgejo / Codeberg / Gitea via OAuth or PAT.",
  },
];

// URL parameter helpers — the wizard's state machine is fully driven by
// ?step=/?installed=/?connected=/?provider=/?base= so a page reload or
// back-navigation lands the operator exactly where they left off.
function readQuery(search: string): URLSearchParams {
  return new URLSearchParams(search);
}

function withQuery(params: URLSearchParams): string {
  const s = params.toString();
  return s ? `${RETURN_PATH}?${s}` : RETURN_PATH;
}

export default function ConnectRepoWizard() {
  const { activeTeam } = useAuth();
  const teamID = activeTeam?.team_id ?? "";
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  const search = useSearch();
  const [, navigate] = useLocation();
  const q = useMemo(() => readQuery(search), [search]);

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Connect a repository</span>,
    right: activeTeam ? (
      <span className="text-xs text-fg-muted">{activeTeam.team_name}</span>
    ) : null,
  });

  // Cloud-only: repositories, forge connections and OAuth apps are all
  // team-scoped resources with no local-mode equivalent.
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-2xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice
            title="Connect a repository"
            feature="Repository connection"
          />
        </div>
      </div>
    );
  }
  if (!teamID) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-2xl mx-auto p-3 sm:p-6">
          <InlineBanner tone="info" layout="inline">
            Select a team to connect a repository. Forge connections are
            team-scoped.
          </InlineBanner>
        </div>
      </div>
    );
  }

  return <WizardInner teamID={teamID} q={q} navigate={navigate} />;
}

interface WizardInnerProps {
  teamID: string;
  q: URLSearchParams;
  navigate: (to: string, opts?: { replace?: boolean }) => void;
}

function WizardInner({ teamID, q, navigate }: WizardInnerProps) {
  const queryClient = useQueryClient();
  const choose = useActiveRepoStore((s) => s.choose);

  const [connections, setConnections] = useState<ForgeConnection[]>([]);
  const [oauthApps, setOAuthApps] = useState<ForgeOAuthApp[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setError(null);
    try {
      const [conns, apps] = await Promise.all([
        listForgeConnections(teamID),
        listForgeOAuthApps(teamID),
      ]);
      setConnections(conns);
      setOAuthApps(apps);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setLoading(false);
    }
  }, [teamID]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const connectedID = q.get("connected") ?? "";
  const installedAppID = q.get("installed") ?? "";
  const explicitStep = q.get("step") as Step | null;
  const providerParam = (q.get("provider") as ForgeProvider | null) ?? "github";
  const baseParam = q.get("base") ?? "";

  // A returning App-install / OAuth round-trip forces the step; otherwise
  // the URL's ?step= wins, falling back to "provider".
  const step: Step = connectedID
    ? "repos"
    : explicitStep && STEP_ORDER.includes(explicitStep)
      ? explicitStep
      : "provider";

  const gotoStep = (next: Step, extra?: Record<string, string>) => {
    const p = new URLSearchParams();
    p.set("step", next);
    if (extra) for (const [k, v] of Object.entries(extra)) if (v) p.set(k, v);
    navigate(withQuery(p), { replace: false });
  };

  const restart = () => navigate(withQuery(new URLSearchParams()));

  // Bot catalog — repo-capable bots feed EnableRepoPanel just like the
  // existing IntegrationsTab does.
  const allBots = useBotsStore((s) => s.bots);
  const botsWarning = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    void fetchBots();
  }, [fetchBots]);
  const repoBots = useMemo(() => (allBots ?? []).filter(isRepoCapable), [allBots]);

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-2xl mx-auto p-3 sm:p-6 space-y-5">
        <StepIndicator
          steps={STEP_ORDER.map((s) => ({ id: s, label: STEP_LABEL[s] }))}
          current={step}
          ariaLabel="Connect a repository — progress"
        />

        {error && (
          <InlineBanner tone="danger" layout="inline">
            {error}
          </InlineBanner>
        )}
        {botsWarning && step === "repos" && (
          <InlineBanner tone="warning" layout="inline">
            {botsWarning}
          </InlineBanner>
        )}

        {step === "provider" && (
          <ProviderStep
            teamID={teamID}
            oauthApps={oauthApps}
            initialProvider={providerParam}
            initialBase={baseParam}
            onError={setError}
            onNextAuthorize={(provider, base) =>
              gotoStep("authorize", {
                provider,
                base: base ?? "",
              })
            }
            onPatConnected={(conn) =>
              navigate(
                withQuery(new URLSearchParams({ connected: conn.id })),
              )
            }
          />
        )}

        {step === "authorize" && (
          <AuthorizeStep
            teamID={teamID}
            provider={providerParam}
            baseURL={baseParam}
            oauthApps={oauthApps}
            installedAppID={installedAppID}
            onBack={() =>
              gotoStep("provider", {
                provider: providerParam,
                base: baseParam,
              })
            }
            onError={setError}
            onPatConnected={(conn) =>
              navigate(
                withQuery(new URLSearchParams({ connected: conn.id })),
              )
            }
          />
        )}

        {step === "repos" && (
          <ReposStep
            teamID={teamID}
            loading={loading}
            connections={connections}
            connectionID={connectedID}
            repoBots={repoBots}
            reloadConnections={reload}
            onError={setError}
            onDone={(enabled) => {
              if (enabled) {
                choose(
                  forgeTeamRepoKey({
                    connection_id: enabled.connectionID,
                    repo_full_name: enabled.repo,
                  }),
                );
              }
              queryClient.invalidateQueries({
                queryKey: ["team-forge-repos", teamID],
              });
              const p = new URLSearchParams({ step: "done" });
              if (enabled) {
                p.set("connected", enabled.connectionID);
                p.set("repo", enabled.repo);
              }
              navigate(withQuery(p));
            }}
          />
        )}

        {step === "done" && (
          <DoneStep
            connectionID={connectedID}
            repo={q.get("repo") ?? ""}
            onGoToRepos={() => navigate("/integrations?tab=forges")}
            onOpenBoard={() => navigate("/board")}
            onLaunchBot={() => navigate("/bots")}
            onConnectAnother={restart}
          />
        )}
      </div>
    </div>
  );
}

/* --------------------------- provider step -------------------------- */

interface ProviderStepProps {
  teamID: string;
  oauthApps: ForgeOAuthApp[];
  initialProvider: ForgeProvider;
  initialBase: string;
  onError: (m: string) => void;
  onNextAuthorize: (provider: ForgeProvider, base: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

function ProviderStep({
  teamID,
  oauthApps,
  initialProvider,
  initialBase,
  onError,
  onNextAuthorize,
  onPatConnected,
}: ProviderStepProps) {
  const [provider, setProvider] = useState<ForgeProvider>(
    CONNECTABLE.includes(initialProvider) ? initialProvider : "github",
  );
  const [baseURL, setBaseURL] = useState<string>(initialBase);
  const [showSelfHosted, setShowSelfHosted] = useState<boolean>(!!initialBase);

  // The self-hosted disclosure is provider-scoped; switching provider without
  // changing the base URL still hides the panel when it's empty.
  const selfHostedApplicable = provider !== "github" || showSelfHosted;

  return (
    <div className="space-y-4">
      <header>
        <h2 className="text-headline font-semibold">Connect a repository</h2>
        <p className="text-xs text-fg-muted mt-1">
          Choose a forge to authorize iterion against. You'll pick which
          repository to wire up in the next step.
        </p>
      </header>

      <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
        {PROVIDER_META.map((p) => {
          const disabled = !CONNECTABLE.includes(p.id);
          const active = provider === p.id;
          return (
            <button
              key={p.id}
              type="button"
              onClick={() => !disabled && setProvider(p.id)}
              disabled={disabled}
              aria-pressed={active}
              className={[
                "text-left rounded-[var(--radius-lg)] border p-3 transition-colors",
                active
                  ? "border-accent bg-accent-soft"
                  : "border-border-default bg-surface-1 hover:border-border-strong",
                disabled ? "opacity-60 cursor-not-allowed" : "cursor-pointer",
              ].join(" ")}
            >
              <div className="flex items-center gap-2">
                <span className="font-medium text-sm">{p.title}</span>
                {p.recommended && (
                  <Badge variant="accent" size="sm">
                    Recommended
                  </Badge>
                )}
              </div>
              <p className="text-caption text-fg-muted mt-1">{p.blurb}</p>
            </button>
          );
        })}
      </div>

      <div className="space-y-2">
        {provider !== "github" || showSelfHosted ? (
          <div>
            <label
              htmlFor="wizard-base-url"
              className="block text-caption text-fg-muted mb-1"
            >
              {provider === "github"
                ? "GitHub Enterprise base URL"
                : "Self-hosted base URL"}
            </label>
            <Input
              size="md"
              id="wizard-base-url"
              placeholder={
                provider === "github"
                  ? "https://github.example.com"
                  : provider === "gitlab"
                    ? "https://gitlab.example.com"
                    : "https://codeberg.org"
              }
              value={baseURL}
              onChange={(e) => setBaseURL(e.target.value)}
              autoComplete="off"
            />
            <p className="text-caption text-fg-subtle mt-1">
              Leave blank for {DEFAULT_BASE[provider]}.
            </p>
          </div>
        ) : (
          <button
            type="button"
            className="text-caption text-accent-text hover:underline"
            onClick={() => setShowSelfHosted(true)}
          >
            Self-hosted instance?
          </button>
        )}
        {!selfHostedApplicable && null}
      </div>

      {/* PAT fallback — always available, subdued behind a disclosure so the
          primary OAuth/App path stays the visual centre. */}
      <PatFallback
        teamID={teamID}
        provider={provider}
        baseURL={baseURL}
        oauthApps={oauthApps}
        onError={onError}
        onConnected={onPatConnected}
      />

      <div className="flex items-center justify-end pt-2">
        <Button
          variant="primary"
          onClick={() => onNextAuthorize(provider, baseURL.trim())}
        >
          Continue
        </Button>
      </div>
    </div>
  );
}

/* --------------------------- PAT fallback --------------------------- */

interface PatFallbackProps {
  teamID: string;
  provider: ForgeProvider;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  onError: (m: string) => void;
  onConnected: (conn: ForgeConnection) => void;
  // bare renders the OAuth/PAT blocks directly, without PatFallback's own
  // disclosure — for callers (NonAppMethods) that already gate visibility
  // behind their own "Other methods" toggle. One toggle, not two.
  bare?: boolean;
}

// PatFallback keeps the PAT path always reachable (self-hosted forges,
// no OAuth app registered, or the operator preferring a token). It also
// offers the OAuth "Use OAuth" secondary path when a matching OAuth app
// is registered — this is the "Other methods" pocket described in the
// wizard brief.
function PatFallback({
  teamID,
  provider,
  baseURL,
  oauthApps,
  onError,
  onConnected,
  bare = false,
}: PatFallbackProps) {
  const [expanded, setExpanded] = useState(false);
  const open = bare || expanded;
  const [busy, setBusy] = useState(false);
  const [pat, setPat] = useState("");

  const oauthAvailable = useMemo(() => {
    return oauthApps.some(
      (a) =>
        a.provider === provider &&
        (a.forge_base_url ?? DEFAULT_BASE[provider]) ===
          canonicalBase(provider, baseURL),
    );
  }, [oauthApps, provider, baseURL]);

  const kickOff = async (mode: ConnectForgeInput["mode"]) => {
    setBusy(true);
    try {
      const res = await connectForge(teamID, {
        provider,
        mode,
        forge_base_url: baseURL.trim() || undefined,
        pat: mode === "pat" ? pat.trim() : undefined,
        next: RETURN_PATH,
      });
      if (res.authorize_url) {
        window.location.href = res.authorize_url;
        return;
      }
      if (res.connection) {
        setPat("");
        onConnected(res.connection);
      }
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className={bare ? "" : "border-t border-border-subtle pt-3"}>
      {!bare && (
        <button
          type="button"
          className="text-caption text-fg-muted hover:text-fg-default"
          aria-expanded={expanded}
          onClick={() => setExpanded((v) => !v)}
        >
          {expanded ? "Hide other methods" : "Other methods (OAuth, personal token)"}
        </button>
      )}
      {open && (
        <div className="mt-2 space-y-3 rounded border border-border-subtle bg-surface-1 p-3">
          {oauthAvailable && (
            <div>
              <div className="text-xs font-medium text-fg-default mb-1">
                Use OAuth
              </div>
              <p className="text-caption text-fg-muted mb-2">
                Authorize the OAuth app registered for this instance. You'll
                be redirected to {provider} and back here.
              </p>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void kickOff("oauth")}
                disabled={busy}
                loading={busy}
              >
                Continue with OAuth
              </Button>
            </div>
          )}
          <div>
            <div className="text-xs font-medium text-fg-default mb-1">
              Personal access token
            </div>
            <p className="text-caption text-fg-muted mb-2">
              Paste a token with the scopes your bots need. The token is
              sealed at rest and used only for this connection.
            </p>
            <Textarea
              value={pat}
              onChange={(e) => setPat(e.target.value)}
              placeholder="Personal access token"
              rows={2}
              className="w-full font-mono text-xs"
              autoComplete="off"
            />
            <div className="mt-2">
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void kickOff("pat")}
                disabled={busy || pat.trim() === ""}
                loading={busy}
              >
                Connect with token
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

/* --------------------------- authorize step ------------------------- */

interface AuthorizeStepProps {
  teamID: string;
  provider: ForgeProvider;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  installedAppID: string;
  onBack: () => void;
  onError: (m: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

function AuthorizeStep({
  teamID,
  provider,
  baseURL,
  oauthApps,
  installedAppID,
  onBack,
  onError,
  onPatConnected,
}: AuthorizeStepProps) {
  if (provider === "github") {
    return (
      <GitHubAuthorize
        teamID={teamID}
        baseURL={baseURL}
        oauthApps={oauthApps}
        installedAppID={installedAppID}
        onBack={onBack}
        onError={onError}
        onPatConnected={onPatConnected}
      />
    );
  }
  return (
    <NonGitHubAuthorize
      teamID={teamID}
      provider={provider}
      baseURL={baseURL}
      oauthApps={oauthApps}
      onBack={onBack}
      onError={onError}
      onPatConnected={onPatConnected}
    />
  );
}

/* --------------------------- GitHub authorize ----------------------- */

interface GitHubAuthorizeProps {
  teamID: string;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  installedAppID: string;
  onBack: () => void;
  onError: (m: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

function GitHubAuthorize({
  teamID,
  baseURL,
  oauthApps,
  installedAppID,
  onBack,
  onError,
  onPatConnected,
}: GitHubAuthorizeProps) {
  const platformAppConfigured = useServerInfoStore(
    (s) => s.info?.forge_github_app_configured ?? false,
  );
  const [busy, setBusy] = useState(false);

  // A team-installable GitHub App exists when a manifest-created app is
  // registered for the (github, base) tuple with installable: true.
  const teamInstallable = useMemo(() => {
    const base = canonicalBase("github", baseURL);
    return oauthApps.find(
      (a) =>
        a.provider === "github" &&
        (a.forge_base_url ?? DEFAULT_BASE.github) === base &&
        a.installable === true,
    );
  }, [oauthApps, baseURL]);

  const appAvailable = platformAppConfigured || !!teamInstallable;

  const installApp = async () => {
    setBusy(true);
    try {
      const res = await connectForge(teamID, {
        provider: "github",
        mode: "app",
        forge_base_url: baseURL.trim() || undefined,
        next: RETURN_PATH,
      });
      if (res.install_url) {
        window.location.href = res.install_url;
        return;
      }
      if (res.authorize_url) {
        window.location.href = res.authorize_url;
        return;
      }
      if (res.connection) {
        onPatConnected(res.connection);
      }
    } catch (e) {
      onError(errorMessage(e));
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4">
      <header className="space-y-1">
        <h2 className="text-headline font-semibold">Authorize GitHub</h2>
        <p className="text-xs text-fg-muted">
          {baseURL.trim()
            ? `Base: ${baseURL.trim()}`
            : "github.com — least-privilege GitHub App is the recommended path."}
        </p>
      </header>

      {installedAppID && (
        <InlineBanner tone="success" layout="inline" title="GitHub App created">
          The app is registered for this team. Install it on GitHub next to
          pick which repositories it can act on.
        </InlineBanner>
      )}

      {appAvailable ? (
        <div className="rounded-[var(--radius-lg)] border border-accent/40 bg-accent-soft p-4 space-y-2">
          <div className="flex items-center gap-2">
            <span className="font-medium">Install the GitHub App</span>
            <Badge variant="accent" size="sm">
              Recommended
            </Badge>
          </div>
          <p className="text-caption text-fg-muted">
            Least privilege — the App gets only the repositories you select
            and the minimal permission set (contents, pull requests,
            webhooks). No account-wide <code>repo</code> scope.
          </p>
          <div>
            <Button
              variant="primary"
              onClick={() => void installApp()}
              disabled={busy}
              loading={busy}
            >
              {installedAppID ? "Install it now on GitHub" : "Install the GitHub App"}
            </Button>
          </div>
        </div>
      ) : (
        <CreateGitHubAppCard
          teamID={teamID}
          baseURL={baseURL}
          onError={onError}
        />
      )}

      <NonAppMethods
        teamID={teamID}
        provider="github"
        baseURL={baseURL}
        oauthApps={oauthApps}
        onError={onError}
        onPatConnected={onPatConnected}
      />

      <div className="flex items-center justify-between pt-2">
        <Button variant="ghost" size="sm" onClick={onBack}>
          ← Back
        </Button>
        <span className="text-caption text-fg-subtle">
          Waiting on a redirect from GitHub after you authorize.
        </span>
      </div>
    </div>
  );
}

/* --------------------- Create GitHub App (manifest) ----------------- */

interface CreateGitHubAppCardProps {
  teamID: string;
  baseURL: string;
  onError: (m: string) => void;
}

function CreateGitHubAppCard({
  teamID,
  baseURL,
  onError,
}: CreateGitHubAppCardProps) {
  const [busy, setBusy] = useState(false);
  const [orgs, setOrgs] = useState<string[]>([]);
  const [orgIsCustom, setOrgIsCustom] = useState(false);
  const [githubOrg, setGithubOrg] = useState("");
  const [allowRepoCreation, setAllowRepoCreation] = useState(true);
  const loadedRef = useRef(false);

  useEffect(() => {
    if (loadedRef.current) return;
    loadedRef.current = true;
    let cancelled = false;
    getMyGitHubOrgs()
      .then((o) => {
        if (!cancelled) setOrgs(o);
      })
      .catch(() => {
        /* silent — the picker link below covers the empty case */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const pickOrgs = async () => {
    try {
      const { authorize_url } = await startGitHubOrgsPicker(RETURN_PATH);
      window.location.href = authorize_url;
    } catch (e) {
      onError(errorMessage(e));
    }
  };

  const create = async () => {
    setBusy(true);
    try {
      const { post_url, manifest } = await startGitHubManifest(teamID, {
        forge_base_url: baseURL.trim() || undefined,
        github_org: githubOrg.trim() || undefined,
        allow_repo_creation: allowRepoCreation,
        next: RETURN_PATH,
      });
      // Auto-submit the hidden form: GitHub swallows the manifest, creates
      // the App, and 302s back to our callback (which redirects us here
      // with ?installed=<oauth_app_id>).
      const form = document.createElement("form");
      form.method = "POST";
      form.action = post_url;
      const field = document.createElement("input");
      field.type = "hidden";
      field.name = "manifest";
      field.value = JSON.stringify(manifest);
      form.appendChild(field);
      document.body.appendChild(form);
      form.submit();
    } catch (e) {
      onError(errorMessage(e));
      setBusy(false);
    }
  };

  return (
    <div className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 p-4 space-y-3">
      <div>
        <div className="font-medium text-sm">Create your GitHub App</div>
        <p className="text-caption text-fg-muted mt-0.5">
          GitHub creates the App with the manifest iterion sends, then brings
          you back here. One click, no admin token.
        </p>
      </div>

      <div className="space-y-1">
        <label
          htmlFor="wizard-gh-org"
          className="block text-caption text-fg-muted"
        >
          Create under
        </label>
        <div className="flex items-center gap-2">
          <Select
            size="md"
            id="wizard-gh-org"
            value={orgIsCustom ? "__other__" : githubOrg}
            onChange={(e) => {
              const v = e.target.value;
              if (v === "__other__") {
                setOrgIsCustom(true);
                setGithubOrg("");
              } else {
                setOrgIsCustom(false);
                setGithubOrg(v);
              }
            }}
          >
            <option value="">Your personal account</option>
            {orgs.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
            <option value="__other__">Other organization…</option>
          </Select>
          <button
            type="button"
            className="text-caption text-accent-text hover:underline shrink-0"
            onClick={() => void pickOrgs()}
          >
            {orgs.length > 0 ? "Refresh from GitHub" : "Pick from GitHub"}
          </button>
        </div>
        {orgIsCustom && (
          <Input
            size="md"
            placeholder="Org login (e.g. SocialGouv)"
            value={githubOrg}
            onChange={(e) => setGithubOrg(e.target.value)}
            autoComplete="off"
          />
        )}
      </div>

      <Checkbox
        id="wizard-allow-repo-creation"
        checked={allowRepoCreation}
        onChange={(e) => setAllowRepoCreation(e.target.checked)}
        label={
          <span className="text-fg-default text-xs">
            <span className="font-medium">
              Allow iterion to create repositories in this org
            </span>
            <span className="block text-caption text-fg-muted mt-0.5">
              Adds the Administration: write permission so bots can bootstrap new
              repositories (recommended). You can revoke this on GitHub later.
            </span>
          </span>
        }
      />

      <Button
        variant="primary"
        onClick={() => void create()}
        disabled={busy}
        loading={busy}
      >
        {busy ? "Opening GitHub…" : "Create a GitHub App"}
      </Button>
    </div>
  );
}

/* --------------------- non-GitHub authorize ------------------------- */

interface NonGitHubAuthorizeProps {
  teamID: string;
  provider: ForgeProvider;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  onBack: () => void;
  onError: (m: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

function NonGitHubAuthorize({
  teamID,
  provider,
  baseURL,
  oauthApps,
  onBack,
  onError,
  onPatConnected,
}: NonGitHubAuthorizeProps) {
  const oauthAvailable = useMemo(() => {
    const base = canonicalBase(provider, baseURL);
    return oauthApps.some(
      (a) =>
        a.provider === provider &&
        (a.forge_base_url ?? DEFAULT_BASE[provider]) === base,
    );
  }, [oauthApps, provider, baseURL]);

  const [busy, setBusy] = useState(false);

  const kickOffOAuth = async () => {
    setBusy(true);
    try {
      const res = await connectForge(teamID, {
        provider,
        mode: "oauth",
        forge_base_url: baseURL.trim() || undefined,
        next: RETURN_PATH,
      });
      if (res.authorize_url) {
        window.location.href = res.authorize_url;
        return;
      }
      if (res.connection) {
        onPatConnected(res.connection);
      }
    } catch (e) {
      onError(errorMessage(e));
      setBusy(false);
    }
  };

  const label = provider === "gitlab" ? "GitLab" : "Forgejo";

  return (
    <div className="space-y-4">
      <header className="space-y-1">
        <h2 className="text-headline font-semibold">Authorize {label}</h2>
        <p className="text-xs text-fg-muted">
          {baseURL.trim() ? `Base: ${baseURL.trim()}` : DEFAULT_BASE[provider]}
        </p>
      </header>

      {oauthAvailable ? (
        <div className="rounded-[var(--radius-lg)] border border-accent/40 bg-accent-soft p-4 space-y-2">
          <div className="font-medium">Continue with OAuth</div>
          <p className="text-caption text-fg-muted">
            Authorize the OAuth app registered for this instance. You'll be
            redirected to {label} and back here.
          </p>
          <div>
            <Button
              variant="primary"
              onClick={() => void kickOffOAuth()}
              disabled={busy}
              loading={busy}
            >
              Continue with OAuth
            </Button>
          </div>
        </div>
      ) : (
        <InlineBanner tone="info" layout="inline">
          No OAuth app is registered for this instance. You can still connect
          with a personal access token below, or register an OAuth app first
          from the Integrations page.
        </InlineBanner>
      )}

      <NonAppMethods
        teamID={teamID}
        provider={provider}
        baseURL={baseURL}
        oauthApps={oauthApps}
        // NonGitHubAuthorize primary flow is OAuth, so hide the redundant
        // "Use OAuth" secondary block; keep the PAT one always visible.
        hideOAuth
        onError={onError}
        onPatConnected={onPatConnected}
      />

      <div className="flex items-center justify-between pt-2">
        <Button variant="ghost" size="sm" onClick={onBack}>
          ← Back
        </Button>
        <span className="text-caption text-fg-subtle">
          Waiting on a redirect from {label} after you authorize.
        </span>
      </div>
    </div>
  );
}

/* --------------------- shared "other methods" block ---------------- */

interface NonAppMethodsProps {
  teamID: string;
  provider: ForgeProvider;
  baseURL: string;
  oauthApps: ForgeOAuthApp[];
  hideOAuth?: boolean;
  onError: (m: string) => void;
  onPatConnected: (conn: ForgeConnection) => void;
}

function NonAppMethods({
  teamID,
  provider,
  baseURL,
  oauthApps,
  hideOAuth = false,
  onError,
  onPatConnected,
}: NonAppMethodsProps) {
  const [expanded, setExpanded] = useState(false);
  return (
    <div className="border-t border-border-subtle pt-3">
      <button
        type="button"
        className="text-caption text-fg-muted hover:text-fg-default"
        aria-expanded={expanded}
        onClick={() => setExpanded((v) => !v)}
      >
        {expanded ? "Hide other methods" : "Other methods (personal token)"}
      </button>
      {expanded && (
        <div className="mt-2">
          <PatFallback
            teamID={teamID}
            provider={provider}
            baseURL={baseURL}
            oauthApps={hideOAuth ? [] : oauthApps}
            onError={onError}
            onConnected={onPatConnected}
            bare
          />
        </div>
      )}
    </div>
  );
}

/* --------------------------- repos step ----------------------------- */

interface ReposStepProps {
  teamID: string;
  loading: boolean;
  connections: ForgeConnection[];
  connectionID: string;
  repoBots: BotEntryWithSchema[];
  reloadConnections: () => Promise<void>;
  onError: (m: string) => void;
  onDone: (enabled?: { repo: string; connectionID: string }) => void;
}

function ReposStep({
  teamID,
  loading,
  connections,
  connectionID,
  repoBots,
  reloadConnections,
  onError,
  onDone,
}: ReposStepProps) {
  const conn = useMemo(
    () => connections.find((c) => c.id === connectionID) ?? null,
    [connections, connectionID],
  );

  // The freshly-connected id may not be in the first list snapshot yet
  // (the connect callback races with our reload). Retry once so we don't
  // dead-end on a legitimate late-arriving connection.
  const retriedRef = useRef(false);
  useEffect(() => {
    if (!loading && !conn && connectionID && !retriedRef.current) {
      retriedRef.current = true;
      void reloadConnections();
    }
  }, [loading, conn, connectionID, reloadConnections]);

  if (loading && !conn) {
    return (
      <EmptyState
        title="Fetching your new connection…"
        message="This usually takes a second."
      />
    );
  }

  if (!conn) {
    return (
      <div className="space-y-3">
        <InlineBanner tone="warning" layout="inline">
          We couldn't find the connection that was just authorized (id{" "}
          <span className="font-mono">{connectionID || "?"}</span>). Try
          reloading — it may still be propagating.
        </InlineBanner>
        <Button
          variant="secondary"
          size="sm"
          onClick={() => {
            retriedRef.current = false;
            void reloadConnections();
          }}
        >
          Retry
        </Button>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      <header className="space-y-1">
        <h2 className="text-headline font-semibold">Pick a repository</h2>
        <div className="flex items-center gap-2 text-xs text-fg-muted">
          <Badge variant="success" size="sm">
            Connected
          </Badge>
          <span>
            {conn.provider} · @{conn.account_login ?? "—"}
            {conn.forge_base_url ? ` · ${conn.forge_base_url}` : ""}
          </span>
        </div>
      </header>

      <EnableRepoPanel
        teamID={teamID}
        conn={conn}
        repoBots={repoBots}
        onDone={(enabled) => onDone(enabled)}
        onCancel={() => onDone()}
        onError={onError}
      />
    </div>
  );
}

/* ---------------------------- done step ----------------------------- */

interface DoneStepProps {
  connectionID: string;
  repo: string;
  onGoToRepos: () => void;
  onOpenBoard: () => void;
  onLaunchBot: () => void;
  onConnectAnother: () => void;
}

function DoneStep({
  connectionID,
  repo,
  onGoToRepos,
  onOpenBoard,
  onLaunchBot,
  onConnectAnother,
}: DoneStepProps) {
  return (
    <div className="space-y-4">
      <header className="space-y-1">
        <h2 className="text-headline font-semibold">
          <span className="inline-flex items-center gap-2">
            <span
              aria-hidden
              className="inline-flex h-6 w-6 items-center justify-center rounded-full bg-success-soft text-success-fg"
            >
              <CheckIcon className="h-4 w-4" />
            </span>
            Repository connected
          </span>
        </h2>
        <p className="text-xs text-fg-muted">
          {repo ? (
            <>
              <span className="font-mono">{repo}</span> is now wired to this
              team. Selected bots have been provisioned with the required
              webhooks and tokens.
            </>
          ) : (
            "Your bots have been provisioned. You can enable more repositories on this connection any time from the Integrations page."
          )}
        </p>
      </header>

      {connectionID && (
        <div className="text-caption text-fg-subtle">
          Connection <span className="font-mono">{connectionID}</span>
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button variant="primary" onClick={onGoToRepos}>
          Go to Repositories
        </Button>
        <Button variant="secondary" onClick={onOpenBoard}>
          Open the board
        </Button>
        <Button variant="secondary" onClick={onLaunchBot}>
          Launch a bot
        </Button>
        <Button variant="ghost" onClick={onConnectAnother}>
          Connect another
        </Button>
      </div>
    </div>
  );
}
