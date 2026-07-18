import { Link } from "wouter";
import { CheckIcon, ChevronRightIcon } from "@radix-ui/react-icons";

import { forgeTeamRepoKey, type ForgeTeamRepo } from "@/api/forgeConnections";
import { Badge } from "@/components/ui/Badge";
import { repoDetailPath } from "@/views/RepoDetail/repoKey";

// CloudReposPanel is the cloud Home counterpart of the local
// RecentFilesPanel: the team's connected repositories, with the active
// one marked. Clicking a row makes it the active repo (the UI scope
// every repo-aware view follows); Manage links to the Integrations
// page.
export default function CloudReposPanel({
  repos,
  activeRepo,
  choose,
}: {
  repos: ForgeTeamRepo[];
  activeRepo: ForgeTeamRepo | null;
  choose: (key: string | null) => void;
}) {
  return (
    <section className="flex flex-col bg-surface-1 border border-border-default rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] overflow-hidden">
      <header className="px-4 py-2.5 border-b border-border-default flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-fg-muted">
          Repositories
        </h2>
        <Link
          href="/integrations"
          className="text-micro text-accent-text hover:underline"
        >
          Manage
        </Link>
      </header>
      {repos.length === 0 ? (
        <div className="p-4 text-label text-fg-muted">
          No repository connected yet.{" "}
          <Link href="/integrations/connect" className="text-accent-text hover:underline">
            Connect one
          </Link>{" "}
          to put bots to work.
        </div>
      ) : (
        <ul className="divide-y divide-border-subtle">
          {repos.map((r) => {
            const key = forgeTeamRepoKey(r);
            const isActive =
              activeRepo !== null && forgeTeamRepoKey(activeRepo) === key;
            const botCount = r.bot_ids?.length ?? 0;
            return (
              <li key={key} className="flex items-center">
                <button
                  type="button"
                  onClick={() => choose(key)}
                  className={`min-w-0 flex-1 pl-4 pr-1 py-2 flex items-center gap-2 text-left hover:bg-surface-2 transition-colors ${
                    isActive ? "bg-surface-2/60" : ""
                  }`}
                  title={
                    isActive
                      ? "This is the active repository"
                      : "Make this the active repository"
                  }
                >
                  <span className="min-w-0 flex-1 truncate font-mono text-label text-fg-default">
                    {r.repo_full_name}
                  </span>
                  <Badge variant="neutral" size="sm">
                    {r.provider}
                  </Badge>
                  <span className="shrink-0 text-caption text-fg-subtle">
                    {botCount} bot{botCount === 1 ? "" : "s"}
                  </span>
                  {isActive && (
                    <span className="inline-flex items-center gap-1 text-caption text-success-fg">
                      <CheckIcon className="h-3 w-3" /> active
                    </span>
                  )}
                </button>
                <Link
                  href={repoDetailPath(r)}
                  className="shrink-0 self-stretch flex items-center px-2 text-fg-subtle hover:bg-surface-2 hover:text-fg-default transition-colors"
                  title={`Repository details — ${r.repo_full_name}`}
                  aria-label={`Repository details — ${r.repo_full_name}`}
                >
                  <ChevronRightIcon className="h-4 w-4" />
                </Link>
              </li>
            );
          })}
        </ul>
      )}
      <footer className="mt-auto px-4 py-2 border-t border-border-subtle">
        <Link
          href="/integrations/connect"
          className="text-micro text-accent-text hover:underline"
        >
          + Connect a repo
        </Link>
      </footer>
    </section>
  );
}
