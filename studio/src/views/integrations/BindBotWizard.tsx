import { useEffect, useMemo, useState } from "react";
import { Link, useLocation, useSearch } from "wouter";
import { CheckIcon } from "@radix-ui/react-icons";
import { useQueryClient } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import {
  type ForgeEnablePreview,
  type ForgeTeamRepo,
  enableForgeRepoBots,
  forgeTeamRepoKey,
  previewForgeEnable,
} from "@/api/forgeConnections";
import { listTeamSecrets } from "@/api/secrets";
import BotIdentity from "@/components/shared/BotIdentity";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import { useAuth } from "@/auth/AuthContext";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { errorMessage } from "@/lib/errorHints";
import { forgeLabel } from "@/lib/forge";
import { isRepoCapable } from "@/lib/triggers";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";
import { botLaunchFile } from "@/views/Bots/botPaths";
import { repoDetailPath } from "@/views/RepoDetail/repoKey";

import {
  BIND_STEP_LABEL,
  BIND_STEP_ORDER,
  type BindPreviewModel,
  type BindStep,
  buildBindPreviewModel,
  firstIncompleteBindStep,
  prevBindStep,
  resolveBindStep,
  sanitizeReturnTo,
  unionBotIds,
} from "./wizard/bindModel";
import { StepIndicator } from "./wizard/StepIndicator";

// Guided bind-bot wizard — /integrations/bind?repo=<key>&bot=<name>&returnTo=<path>.
// Mirrors ConnectRepoWizard's URL-driven step machine: Repository → Bot →
// Review (dry-run preview) → Done. Prefilled steps are auto-skipped, so a
// bot page lands straight on the repo picker and a repo page on the bot
// picker. All transitions live in wizard/bindModel.ts.

const BIND_PATH = "/integrations/bind";

export default function BindBotWizard() {
  const { activeTeam } = useAuth();
  const teamID = activeTeam?.team_id ?? "";
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  const search = useSearch();
  const [, navigate] = useLocation();
  const q = useMemo(() => new URLSearchParams(search), [search]);

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Bind a bot</span>,
    right: activeTeam ? (
      <span className="text-xs text-fg-muted">{activeTeam.team_name}</span>
    ) : null,
  });

  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-2xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice title="Bind a bot" feature="Repository bot binding" />
        </div>
      </div>
    );
  }
  if (!teamID) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-2xl mx-auto p-3 sm:p-6">
          <InlineBanner tone="info" layout="inline">
            Select a team to bind a bot. Connected repositories are team-scoped.
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
  const { repos, loading: reposLoading } = useActiveRepo();

  const allBots = useBotsStore((s) => s.bots);
  const botsWarning = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);
  useEffect(() => {
    void fetchBots();
  }, [fetchBots]);
  const repoBots = useMemo(() => (allBots ?? []).filter(isRepoCapable), [allBots]);

  const repoKey = q.get("repo") ?? "";
  const botName = q.get("bot") ?? "";
  const returnTo = sanitizeReturnTo(q.get("returnTo"));

  // Which params arrived as ENTRY prefills (vs picked in the wizard) —
  // captured on mount so Back skips only the steps the entry point fixed.
  const [prefill] = useState(() => ({ repo: !!repoKey, bot: !!botName }));

  const step = resolveBindStep({ step: q.get("step"), repo: repoKey, bot: botName });

  const gotoStep = (next: BindStep, extra?: { repo?: string; bot?: string }) => {
    const p = new URLSearchParams();
    p.set("step", next);
    const repo = extra?.repo ?? repoKey;
    const bot = extra?.bot ?? botName;
    if (repo) p.set("repo", repo);
    if (bot) p.set("bot", bot);
    if (returnTo) p.set("returnTo", returnTo);
    navigate(`${BIND_PATH}?${p.toString()}`);
  };

  // Cheap lookups — no memoization needed (small lists, every render).
  const selectedRepo = repos.find((r) => forgeTeamRepoKey(r) === repoKey) ?? null;
  const botEntry = (allBots ?? []).find((b) => b.name === botName) ?? null;

  // Team secret names for the review step's missing-secret flags; null =
  // unknown (endpoint unavailable) — never flag on unknown data.
  const [secretNames, setSecretNames] = useState<string[] | null>(null);
  useEffect(() => {
    let cancelled = false;
    listTeamSecrets(teamID)
      .then((list) => {
        if (!cancelled) setSecretNames(list.map((s) => s.name));
      })
      .catch(() => {
        if (!cancelled) setSecretNames(null);
      });
    return () => {
      cancelled = true;
    };
  }, [teamID]);

  const [error, setError] = useState<string | null>(null);
  // Set when the enable came back 202: the org requires an admin approval,
  // so the done step must not claim the bot is live.
  const [pendingApproval, setPendingApproval] = useState(false);
  const back = prevBindStep(step, prefill);

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-2xl mx-auto p-3 sm:p-6 space-y-5">
        <StepIndicator
          steps={BIND_STEP_ORDER.map((s) => ({ id: s, label: BIND_STEP_LABEL[s] }))}
          current={step}
          ariaLabel="Bind a bot — progress"
        />

        {error && (
          <InlineBanner tone="danger" layout="inline">
            {error}
          </InlineBanner>
        )}
        {botsWarning && step === "bot" && (
          <InlineBanner tone="warning" layout="inline">
            {botsWarning}
          </InlineBanner>
        )}

        {step === "repo" && (
          <RepoStep
            repos={repos}
            loading={reposLoading}
            botLabel={botEntry?.display_name?.trim() || botName}
            onPick={(key) =>
              gotoStep(firstIncompleteBindStep(true, !!botName), { repo: key })
            }
          />
        )}

        {step === "bot" && selectedRepo && (
          <BotStep
            repo={selectedRepo}
            bots={repoBots}
            loading={allBots === null}
            onPick={(name) => gotoStep("review", { bot: name })}
            onBack={back ? () => gotoStep(back) : undefined}
          />
        )}

        {step === "review" && selectedRepo && (
          <ReviewStep
            teamID={teamID}
            repo={selectedRepo}
            botName={botName}
            botEntry={botEntry}
            secretNames={secretNames}
            onBack={back ? () => gotoStep(back) : undefined}
            onError={setError}
            onEnabled={(pending) => {
              setPendingApproval(pending);
              queryClient.invalidateQueries({
                queryKey: ["team-forge-repos", teamID],
              });
              gotoStep("done");
            }}
          />
        )}

        {step === "done" && selectedRepo && (
          <DoneStep
            repo={selectedRepo}
            botName={botName}
            botEntry={botEntry}
            returnTo={returnTo}
            navigate={navigate}
            pendingApproval={pendingApproval}
          />
        )}

        {(step === "bot" || step === "review" || step === "done") &&
          !selectedRepo &&
          (reposLoading ? (
            <div className="flex items-center gap-2 text-sm text-fg-muted">
              <Spinner /> Loading repositories…
            </div>
          ) : (
            <InlineBanner tone="warning" layout="inline">
              The selected repository isn't connected to this team any more.{" "}
              <button
                type="button"
                className="text-accent-text hover:underline"
                onClick={() => navigate(BIND_PATH)}
              >
                Pick another
              </button>
              .
            </InlineBanner>
          ))}
      </div>
    </div>
  );
}

/* -------------------------- repository step ------------------------- */

function RepoStep({
  repos,
  loading,
  botLabel,
  onPick,
}: {
  repos: ForgeTeamRepo[];
  loading: boolean;
  /** Non-empty when the bot arrived as a prefill — names it in the intro. */
  botLabel: string;
  onPick: (key: string) => void;
}) {
  if (loading && repos.length === 0) {
    return (
      <div className="flex items-center gap-2 text-sm text-fg-muted">
        <Spinner /> Loading repositories…
      </div>
    );
  }
  if (repos.length === 0) {
    return (
      <EmptyState
        title="No connected repositories"
        message="Connect a repository first — then come back to bind a bot to it."
        action={
          <Link href={`/integrations/connect?returnTo=${encodeURIComponent(BIND_PATH)}`}>
            <Button variant="primary" size="sm">
              Connect a repository
            </Button>
          </Link>
        }
      />
    );
  }
  return (
    <div className="space-y-4">
      <header>
        <h2 className="text-headline font-semibold">Pick a repository</h2>
        <p className="text-xs text-fg-muted mt-1">
          {botLabel
            ? `Choose the repository ${botLabel} should work on.`
            : "Choose the repository the bot should work on."}
        </p>
      </header>
      <ul className="grid grid-cols-1 gap-2">
        {repos.map((r) => {
          const count = r.bot_ids.length;
          return (
            <li key={forgeTeamRepoKey(r)}>
              <button
                type="button"
                onClick={() => onPick(forgeTeamRepoKey(r))}
                className="w-full text-left rounded-[var(--radius-lg)] border border-border-default bg-surface-1 p-3 transition-colors hover:border-border-strong cursor-pointer"
              >
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-mono text-sm font-medium text-fg-default">
                    {r.repo_full_name}
                  </span>
                  <Badge variant="neutral" size="sm">
                    {r.provider}
                  </Badge>
                </div>
                <p className="text-caption text-fg-muted mt-1">
                  {count === 0
                    ? "No bots bound yet"
                    : `${count} bot${count === 1 ? "" : "s"} bound`}
                </p>
              </button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

/* ------------------------------ bot step ---------------------------- */

function BotStep({
  repo,
  bots,
  loading,
  onPick,
  onBack,
}: {
  repo: ForgeTeamRepo;
  bots: BotEntryWithSchema[];
  loading: boolean;
  onPick: (name: string) => void;
  onBack?: () => void;
}) {
  if (loading && bots.length === 0) {
    return (
      <div className="flex items-center gap-2 text-sm text-fg-muted">
        <Spinner /> Loading bots…
      </div>
    );
  }
  return (
    <div className="space-y-4">
      <header>
        <h2 className="text-headline font-semibold">Pick a bot</h2>
        <p className="text-xs text-fg-muted mt-1">
          It will react to <span className="font-mono">{repo.repo_full_name}</span>{" "}
          events once bound.
        </p>
      </header>

      {bots.length === 0 ? (
        <EmptyState
          title="No repo-installable bots"
          message={
            <>
              A bot needs an <span className="font-mono">invocations:</span> block
              in its manifest to be bindable to a repository.
            </>
          }
        />
      ) : (
        <ul className="grid grid-cols-1 gap-2">
          {bots.map((b) => {
            const bound = repo.bot_ids.includes(b.name);
            return (
              <li key={b.name}>
                <button
                  type="button"
                  onClick={() => !bound && onPick(b.name)}
                  disabled={bound}
                  className={[
                    "w-full text-left rounded-[var(--radius-lg)] border p-3 transition-colors",
                    bound
                      ? "border-border-default bg-surface-1 opacity-60 cursor-not-allowed"
                      : "border-border-default bg-surface-1 hover:border-border-strong cursor-pointer",
                  ].join(" ")}
                >
                  <BotIdentity
                    bot={b}
                    clampDescription
                    nameExtras={
                      bound && (
                        <Badge variant="neutral" size="sm">
                          Already bound
                        </Badge>
                      )
                    }
                  />
                </button>
              </li>
            );
          })}
        </ul>
      )}

      {onBack && (
        <div className="pt-2">
          <Button variant="ghost" size="sm" onClick={onBack}>
            ← Back
          </Button>
        </div>
      )}
    </div>
  );
}

/* ----------------------------- review step -------------------------- */

function ReviewStep({
  teamID,
  repo,
  botName,
  botEntry,
  secretNames,
  onBack,
  onError,
  onEnabled,
}: {
  teamID: string;
  repo: ForgeTeamRepo;
  botName: string;
  botEntry: BotEntryWithSchema | null;
  secretNames: string[] | null;
  onBack?: () => void;
  onError: (m: string) => void;
  onEnabled: (pendingApproval: boolean) => void;
}) {
  const botIDs = useMemo(() => unionBotIds(repo.bot_ids, botName), [repo.bot_ids, botName]);
  const [preview, setPreview] = useState<ForgeEnablePreview | null>(null);
  const [previewErr, setPreviewErr] = useState<string | null>(null);
  const [previewLoading, setPreviewLoading] = useState(true);
  const [busy, setBusy] = useState(false);

  const connID = repo.connection_id;
  const repoFullName = repo.repo_full_name;
  useEffect(() => {
    let cancelled = false;
    setPreviewLoading(true);
    setPreviewErr(null);
    previewForgeEnable(teamID, connID, repoFullName, botIDs)
      .then((p) => {
        if (!cancelled) setPreview(p);
      })
      .catch((e: unknown) => {
        if (!cancelled) setPreviewErr(errorMessage(e));
      })
      .finally(() => {
        if (!cancelled) setPreviewLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [teamID, connID, repoFullName, botIDs]);

  const model = useMemo(
    () => buildBindPreviewModel(preview, secretNames),
    [preview, secretNames],
  );

  const label = botEntry?.display_name?.trim() || botName;
  const alreadyBound = repo.bot_ids.filter((id) => id !== botName);

  const enable = async () => {
    setBusy(true);
    try {
      const res = await enableForgeRepoBots(teamID, connID, repoFullName, botIDs);
      onEnabled(!!res.pending_approval);
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-4">
      <header className="space-y-1">
        <h2 className="text-headline font-semibold">Review</h2>
        <p className="text-xs text-fg-muted">
          This will enable <span className="font-medium text-fg-default">{label}</span>{" "}
          on <span className="font-mono">{repo.repo_full_name}</span>.
          {alreadyBound.length > 0 && (
            <>
              {" "}
              Already bound and kept: {alreadyBound.join(", ")}.
            </>
          )}
        </p>
      </header>

      {botEntry && <BotIdentity bot={botEntry} clampDescription />}

      {previewLoading ? (
        <div className="flex items-center gap-2 text-sm text-fg-muted">
          <Spinner /> Computing what this bot will subscribe to…
        </div>
      ) : previewErr ? (
        <InlineBanner tone="warning" layout="inline">
          Couldn't compute the preview ({previewErr}) — you can still enable;
          the server validates the request.
        </InlineBanner>
      ) : (
        model && <PreviewPanel model={model} provider={repo.provider} />
      )}

      <div className="flex items-center justify-between pt-2">
        {onBack ? (
          <Button variant="ghost" size="sm" onClick={onBack}>
            ← Back
          </Button>
        ) : (
          <span />
        )}
        <Button
          variant="primary"
          onClick={() => void enable()}
          disabled={busy || previewLoading || (model?.hasConflicts ?? false)}
          loading={busy}
        >
          {busy ? "Enabling…" : `Enable ${label}`}
        </Button>
      </div>
    </div>
  );
}

// PreviewPanel renders the dry-run: events, token scopes, slash-commands,
// secret bindings (missing ones flagged), identity, conflicts. Sections
// with no data are simply omitted.
function PreviewPanel({
  model,
  provider,
}: {
  model: BindPreviewModel;
  provider: string;
}) {
  const wording = forgeLabel(provider);
  return (
    <div className="rounded-[var(--radius-lg)] border border-border-subtle bg-surface-1 p-3 text-xs space-y-2.5">
      {model.events.length > 0 && (
        <PreviewSection title="Will subscribe to">
          <span className="flex flex-wrap gap-1">
            {model.events.map((e) => (
              <Badge key={e} variant="neutral" size="sm">
                <span className="font-mono">{e}</span>
              </Badge>
            ))}
          </span>
        </PreviewSection>
      )}

      {model.scopes.length > 0 && (
        <PreviewSection title="Required token scopes">
          <ul className="space-y-0.5">
            {model.scopes.map((s) => (
              <li key={s.scope}>
                <span className="font-mono text-fg-default">{s.scope}</span>
                {s.reason && <span className="text-fg-muted"> — {s.reason}</span>}
              </li>
            ))}
          </ul>
        </PreviewSection>
      )}

      {model.commands.length > 0 && (
        <PreviewSection title={`${wording.noun} slash-commands`}>
          <ul className="space-y-0.5">
            {model.commands.map((c) => (
              <li key={c.command}>
                <span className="font-mono text-fg-default">/{c.command}</span>
                <span className="text-fg-muted"> → {c.botId}</span>
              </li>
            ))}
          </ul>
        </PreviewSection>
      )}

      {model.secrets.length > 0 && (
        <PreviewSection title="Secret bindings">
          <ul className="space-y-1">
            {model.secrets.map((s) => (
              <li key={`${s.botId}:${s.secret}`} className="flex flex-wrap items-center gap-1.5">
                <span className="font-mono text-fg-default">{s.secret}</span>
                <span className="text-fg-muted">for {s.botId}</span>
                {s.missing && (
                  <span className="text-warning-fg">
                    ⚠ not found in team secrets —{" "}
                    <Link href="/secrets" className="text-accent-text hover:underline">
                      add it
                    </Link>
                  </span>
                )}
              </li>
            ))}
          </ul>
        </PreviewSection>
      )}

      {model.identity && (
        <PreviewSection title="Will post as">
          <span className="text-fg-default">@{model.identity.handle}</span>
          {model.identity.baseUrl && (
            <span className="text-fg-muted"> on {model.identity.baseUrl}</span>
          )}
        </PreviewSection>
      )}

      {model.hasConflicts && (
        <div className="space-y-1">
          {model.conflicts.map((c) => (
            <InlineBanner key={c} tone="danger" layout="inline">
              {c}
            </InlineBanner>
          ))}
        </div>
      )}
    </div>
  );
}

function PreviewSection({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <div className="text-caption font-semibold uppercase tracking-wider text-fg-subtle mb-0.5">
        {title}
      </div>
      {children}
    </div>
  );
}

/* ------------------------------ done step --------------------------- */

function DoneStep({
  repo,
  botName,
  botEntry,
  returnTo,
  navigate,
  pendingApproval,
}: {
  repo: ForgeTeamRepo;
  botName: string;
  botEntry: BotEntryWithSchema | null;
  returnTo: string | null;
  navigate: (to: string) => void;
  pendingApproval: boolean;
}) {
  const label = botEntry?.display_name?.trim() || botName;
  const launchFile = botEntry ? botLaunchFile(botEntry) : null;
  const scheduleHref = `/triggers?tab=schedules&repo=${encodeURIComponent(repo.repo_full_name)}`;
  if (pendingApproval) {
    return (
      <div className="space-y-4">
        <header className="space-y-1">
          <h2 className="text-headline font-semibold">Awaiting org approval</h2>
          <p className="text-xs text-fg-muted">
            Your organization requires an org admin to approve repo
            provisioning. The request to enable {label} on{" "}
            <span className="font-mono">{repo.repo_full_name}</span> is queued —
            nothing is created on the forge until it is approved. It appears
            under the team&apos;s Integrations tab and the org admins&apos;
            approval queue.
          </p>
        </header>
        <div className="flex flex-wrap items-center gap-2">
          <Button
            variant="secondary"
            onClick={() => navigate(returnTo || "/teams")}
          >
            Done
          </Button>
        </div>
      </div>
    );
  }
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
            Bot bound
          </span>
        </h2>
        <p className="text-xs text-fg-muted">
          {label} is now enabled on{" "}
          <span className="font-mono">{repo.repo_full_name}</span> — the webhook
          and tokens it needs are provisioned.
        </p>
      </header>

      <div className="flex flex-wrap items-center gap-2">
        <Button variant="primary" onClick={() => navigate(repoDetailPath(repo))}>
          View repository
        </Button>
        <Button variant="secondary" onClick={() => navigate(scheduleHref)}>
          Add a schedule
        </Button>
        {launchFile && (
          <Button
            variant="secondary"
            onClick={() => navigate(`/runs/new?file=${encodeURIComponent(launchFile)}`)}
          >
            Launch now
          </Button>
        )}
        {returnTo && (
          <Button variant="ghost" onClick={() => navigate(returnTo)}>
            Back to where you came from
          </Button>
        )}
      </div>
    </div>
  );
}
