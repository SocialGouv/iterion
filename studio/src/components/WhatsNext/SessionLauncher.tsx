import { useEffect, useState } from "react";
import { Link } from "wouter";

import type { FirstClassBot } from "@/lib/whats-next/firstClassBots";
import type { FormAnswer } from "@/lib/whats-next/questionForm";
import { Button, InlineBanner, Input } from "@/components/ui";
import { WizardForm } from "@/components/ui/WizardForm";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useServerInfoStore } from "@/store/serverInfo";

interface Props {
  bot: FirstClassBot;
  // Called when the user submits the launcher (form or bare Start)
  // with the launch var map. When the bot declares a `seedVar`, the
  // launcher form's answer text is already folded into the vars under
  // that name (the bot reads it as the first operator message).
  onLaunch?: (params: { vars: Record<string, string> }) => void;
  busy?: boolean;
  errorMessage?: string | null;
  // Startup discovery failed — a live session may exist that we can't
  // see, so launching may double the spend. Rendered with a Retry.
  discoveryError?: string | null;
  onRetryDiscovery?: () => void;
  // The repo the launch will operate on (cloud active repo), or null
  // for a board-only launch.
  launchRepo?: string | null;
}

export default function SessionLauncher({
  bot,
  onLaunch,
  busy = false,
  errorMessage = null,
  discoveryError = null,
  onRetryDiscovery,
  launchRepo = null,
}: Props) {
  const workDir = useServerInfoStore((s) => s.info?.work_dir ?? "");
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const { repos, enabled: repoScopeEnabled } = useActiveRepo();

  // Initialise each launcherVar from its defaultFrom rule (today only
  // `work_dir`). User can override before submitting.
  const [vars, setVars] = useState<Record<string, string>>(() => {
    const out: Record<string, string> = {};
    for (const v of bot.launcherVars) {
      out[v.name] = v.defaultFrom === "work_dir" ? workDir : "";
    }
    return out;
  });

  // Keep the work_dir defaults in sync if server info loads after
  // first render (initial fetch may be in-flight when the route
  // mounts).
  useEffect(() => {
    setVars((prev) => {
      const next = { ...prev };
      for (const v of bot.launcherVars) {
        if (v.defaultFrom === "work_dir" && !prev[v.name]) {
          next[v.name] = workDir;
        }
      }
      return next;
    });
  }, [bot.launcherVars, workDir]);

  const varsReady = bot.launcherVars.every(
    (v) => (vars[v.name] ?? "").trim() !== "",
  );
  const launch = (formAnswer?: FormAnswer) => {
    if (!onLaunch || busy || !varsReady) return;
    // Seed-var wiring: the launcher form is a single question whose
    // answer (a canned preset or the operator's own text) becomes the
    // bot's first message via vars[seedVar].
    const next = { ...vars };
    if (bot.seedVar && formAnswer) {
      const first = Object.values(formAnswer)[0];
      const text = Array.isArray(first) ? first.join(", ") : first ?? "";
      if (text.trim() !== "") next[bot.seedVar] = text.trim();
    }
    onLaunch({ vars: next });
  };

  return (
    <div className="max-w-2xl mx-auto py-8 px-4">
      <div className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 shadow-[var(--shadow-lg)] p-6 space-y-4">
        <div>
          <h2 className="text-display font-semibold tracking-tight text-fg-default">{bot.label}</h2>
          <p className="mt-1 text-label text-fg-muted">{bot.description}</p>
        </div>

        {discoveryError && (
          <InlineBanner
            tone="warning"
            layout="inline"
            title="Couldn't check for a running session"
            action={
              onRetryDiscovery && (
                <Button variant="secondary" size="sm" onClick={onRetryDiscovery}>
                  Retry
                </Button>
              )
            }
          >
            {discoveryError} — launching now may run two sessions in parallel.
          </InlineBanner>
        )}

        {/* What a session actually does — reads, writes, spend — so the
            first launch isn't a leap of faith. */}
        <ul className="space-y-1 text-caption text-fg-muted border-t border-border-subtle pt-3">
          <li>
            <span className="text-fg-default font-medium">Reads</span> — the
            team board{launchRepo ? " and the repository" : cloud ? " (no repository attached)" : " and the workspace"}.
          </li>
          <li>
            <span className="text-fg-default font-medium">Writes</span> — board
            cards (create, move, close); never your code.
          </li>
          <li>
            <span className="text-fg-default font-medium">Spend</span> — each
            autonomous burst is capped by the bot&apos;s budget; the session
            pauses for you between turns and only spends again on your reply.
          </li>
        </ul>

        {cloud && repoScopeEnabled && (
          launchRepo ? (
            <p className="text-caption text-fg-subtle">
              Operates on{" "}
              <code className="font-mono text-fg-default">{launchRepo}</code> —
              switch repos from the sidebar before launching to change scope.
            </p>
          ) : repos.length === 0 ? (
            <InlineBanner tone="warning" layout="inline">
              No repository connected — the session will only see the board,
              not code.{" "}
              <Link href="/integrations/connect" className="underline">
                Connect a repository
              </Link>{" "}
              first for grounded recommendations.
            </InlineBanner>
          ) : (
            <InlineBanner tone="info" layout="inline">
              &quot;All repos&quot; scope — the session gets the board only.
              Pick a repo in the sidebar to give it code access.
            </InlineBanner>
          )
        )}

        {bot.launcherVars.length > 0 && (
          <div className="space-y-3">
            {bot.launcherVars.map((v) => (
              <div key={v.name} className="space-y-1">
                <label className="text-micro uppercase tracking-wide text-fg-subtle">
                  {v.label}
                </label>
                <Input
                  value={vars[v.name] ?? ""}
                  onChange={(e) =>
                    setVars((prev) => ({ ...prev, [v.name]: e.target.value }))
                  }
                  placeholder={
                    v.defaultFrom === "work_dir" ? "Workspace directory" : ""
                  }
                  disabled={busy}
                />
                {v.defaultFrom === "work_dir" && !cloud && (
                  <p className="text-caption text-fg-subtle">
                    Absolute path. Defaults to the studio&apos;s working directory.
                  </p>
                )}
              </div>
            ))}
          </div>
        )}

        {errorMessage && (
          <p className="text-body text-danger-fg" role="alert">
            {errorMessage}
          </p>
        )}

        {bot.launcherForm ? (
          <WizardForm
            spec={bot.launcherForm}
            busy={busy || !varsReady}
            onSubmit={(answer) => launch(answer)}
          />
        ) : (
          <div className="flex items-center justify-end gap-2 pt-2 border-t border-border-subtle">
            <Button
              variant="primary"
              size="md"
              disabled={busy || !varsReady}
              onClick={() => launch()}
            >
              {busy ? "Starting…" : "Start"}
            </Button>
          </div>
        )}

      </div>
    </div>
  );
}
