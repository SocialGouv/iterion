import { CheckIcon } from "@radix-ui/react-icons";
import { Link } from "wouter";

import type { ForgeTeamRepo } from "@/api/forgeConnections";
import type { RunSummary } from "@/api/runs";

// GettingStartedCard is the cloud golden path, rendered on Home while
// any step is incomplete: connect a repository → enable a bot on it →
// launch a first run. Each step checks off from live state, so the card
// doubles as onboarding progress and disappears once the operator is
// productive (no permanent chrome).
export default function GettingStartedCard({
  repos,
  runs,
}: {
  repos: ForgeTeamRepo[];
  runs: RunSummary[];
}) {
  const hasRepo = repos.length > 0;
  const hasBot = repos.some((r) => (r.bot_ids?.length ?? 0) > 0);
  const hasRun = runs.length > 0;
  if (hasRepo && hasBot && hasRun) return null;

  const steps: Array<{
    label: string;
    detail: string;
    href: string;
    done: boolean;
  }> = [
    {
      label: "Connect a repository",
      detail: "Link a GitHub / GitLab / Forgejo repo to this team.",
      href: "/integrations/connect",
      done: hasRepo,
    },
    {
      label: "Enable a bot on it",
      detail: "Pick which bots may work on the repo.",
      href: "/integrations",
      done: hasBot,
    },
    {
      label: "Launch your first run",
      detail: "Start a bot and watch it work.",
      href: "/bots",
      done: hasRun,
    },
  ];
  const next = steps.find((s) => !s.done);

  return (
    <section className="rounded-[var(--radius-lg)] border border-border-default bg-surface-1 p-4 shadow-[var(--shadow-sm)]">
      <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
        Get started
      </h2>
      <ol className="mt-3 space-y-2">
        {steps.map((s, i) => {
          const isNext = next === s;
          return (
            <li key={s.label} className="flex items-start gap-2.5">
              <span
                aria-hidden
                className={`mt-0.5 flex h-5 w-5 shrink-0 items-center justify-center rounded-full border text-micro font-semibold ${
                  s.done
                    ? "border-success/40 bg-success-soft text-success-fg"
                    : isNext
                      ? "border-accent bg-accent text-accent-fg"
                      : "border-border-default bg-surface-2 text-fg-subtle"
                }`}
              >
                {s.done ? <CheckIcon className="h-3 w-3" /> : i + 1}
              </span>
              <div className="min-w-0 flex-1">
                {s.done ? (
                  <span className="text-label text-fg-muted line-through decoration-fg-subtle/60">
                    {s.label}
                  </span>
                ) : (
                  <Link
                    href={s.href}
                    className={`text-label hover:underline ${
                      isNext ? "font-medium text-accent-text" : "text-fg-default"
                    }`}
                  >
                    {s.label} →
                  </Link>
                )}
                {!s.done && (
                  <p className="text-caption text-fg-subtle">{s.detail}</p>
                )}
              </div>
            </li>
          );
        })}
      </ol>
    </section>
  );
}
