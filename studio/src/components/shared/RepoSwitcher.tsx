import { useMemo, useState } from "react";

import { CaretSortIcon, ChevronRightIcon, PlusIcon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";

import { forgeTeamRepoKey, type ForgeTeamRepo } from "@/api/forgeConnections";
import { Input } from "@/components/ui/Input";
import { Popover, PopoverClose } from "@/components/ui/Popover";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { repoDetailPath } from "@/views/RepoDetail/repoKey";

// Repository switcher, pinned right below the OrgSwitcher: the studio is
// repo-first — one concrete connected repo scopes most views and
// pre-fills repo-targeting actions. "All repos" is the explicit overview
// mode; with nothing connected the switcher IS the onboarding CTA.
// Cloud-only (repos are team-scoped forge integrations).

function repoShortName(fullName: string): string {
  const parts = fullName.split("/").filter(Boolean);
  return parts[parts.length - 1] ?? fullName;
}

function statusTone(status?: string): string | null {
  switch (status) {
    case "degraded":
    case "needs_reauth":
      return "bg-warning";
    case "revoked":
      return "bg-danger";
    default:
      return null;
  }
}

export default function RepoSwitcher({ collapsed = false }: { collapsed?: boolean }) {
  const { activeRepo, overview, repos, enabled, choose } = useActiveRepo();
  const [, navigate] = useLocation();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");

  const byProvider = useMemo(() => {
    const q = search.trim().toLowerCase();
    const filtered = q
      ? repos.filter((r) => r.repo_full_name.toLowerCase().includes(q))
      : repos;
    const groups = new Map<string, ForgeTeamRepo[]>();
    for (const r of filtered) {
      const list = groups.get(r.provider) ?? [];
      list.push(r);
      groups.set(r.provider, list);
    }
    return groups;
  }, [repos, search]);

  if (!enabled) return null;

  const connectHref = "/integrations/connect";
  const goConnect = () => {
    setOpen(false);
    navigate(connectHref);
  };

  // Onboarding: nothing connected yet — the switcher is the CTA.
  if (repos.length === 0) {
    if (collapsed) {
      return (
        <button
          type="button"
          onClick={goConnect}
          className="flex h-8 w-full items-center justify-center rounded hover:bg-surface-2 transition-colors text-accent"
          title="Connect a repository"
          aria-label="Connect a repository"
        >
          <PlusIcon className="h-4 w-4" />
        </button>
      );
    }
    return (
      <button
        type="button"
        onClick={goConnect}
        className="flex w-full items-center gap-2 rounded-md border border-dashed border-border-default bg-surface-0 px-2 py-1.5 text-left hover:bg-surface-2 transition-colors"
        title="Connect a repository to scope the studio on it"
      >
        <span className="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-md bg-accent-soft text-accent-text">
          <PlusIcon className="h-4 w-4" />
        </span>
        <span className="min-w-0 flex-1 leading-tight">
          <span className="block text-caption text-fg-muted">Repository</span>
          <span className="block truncate text-xs font-medium text-accent-text">
            Connect a repo…
          </span>
        </span>
      </button>
    );
  }

  const label = overview
    ? "All repos"
    : activeRepo
      ? repoShortName(activeRepo.repo_full_name)
      : "Select repo";
  const fullLabel = overview ? "All repos" : (activeRepo?.repo_full_name ?? "Select repo");
  const tone = statusTone(activeRepo?.connection_status);

  const pick = (key: string | null) => () => {
    choose(key);
    setOpen(false);
    setSearch("");
  };

  return (
    <Popover
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (!o) setSearch("");
      }}
      side="bottom"
      align="start"
      contentClassName="w-[min(20rem,calc(100vw-1rem))] p-2 text-sm"
      trigger={
        collapsed ? (
          <button
            type="button"
            className="flex h-8 w-full items-center justify-center rounded hover:bg-surface-2 transition-colors"
            title={`Repository: ${fullLabel}`}
            aria-label={`Switch repository — current: ${fullLabel}`}
          >
            <span className="relative inline-flex h-5 w-5 items-center justify-center rounded-md bg-surface-2 text-caption font-semibold uppercase text-fg-default">
              {overview ? "∗" : label.slice(0, 1).toUpperCase()}
              {tone && (
                <span className={`absolute -right-0.5 -top-0.5 h-1.5 w-1.5 rounded-full ${tone}`} />
              )}
            </span>
          </button>
        ) : (
          <button
            type="button"
            className="flex w-full items-center gap-2 rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-left hover:bg-surface-2 transition-colors"
            title={`Repository: ${fullLabel}`}
            aria-label={`Switch repository — current: ${fullLabel}`}
          >
            <span className="relative inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-md bg-surface-2 text-caption font-semibold uppercase text-fg-default">
              {overview ? "∗" : label.slice(0, 1).toUpperCase()}
              {tone && (
                <span className={`absolute -right-0.5 -top-0.5 h-2 w-2 rounded-full border border-surface-0 ${tone}`} />
              )}
            </span>
            <span className="min-w-0 flex-1 leading-tight">
              <span className="block text-caption text-fg-muted">Repository</span>
              <span className="block truncate text-xs font-medium text-fg-default">
                {label}
              </span>
            </span>
            <CaretSortIcon className="h-4 w-4 shrink-0 text-fg-subtle" />
          </button>
        )
      }
    >
      {repos.length > 6 && (
        <div className="px-1 pb-1.5">
          <Input
            size="sm"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Filter repositories…"
            aria-label="Filter repositories"
          />
        </div>
      )}
      <PopoverClose asChild>
        <button
          onClick={pick(null)}
          className={`w-full text-left px-2 py-1.5 rounded hover:bg-surface-2 ${overview ? "bg-surface-2" : ""}`}
        >
          <div className="font-medium">All repos</div>
          <div className="text-xs text-fg-muted">
            Overview — aggregate every connected repo
          </div>
        </button>
      </PopoverClose>
      {Array.from(byProvider.entries()).map(([provider, list]) => (
        <div key={provider}>
          <div className="px-2 pt-2 pb-1 text-xs uppercase tracking-wider text-fg-muted">
            {provider}
          </div>
          {list.map((r) => {
            const key = forgeTeamRepoKey(r);
            const active = !overview && activeRepo != null && forgeTeamRepoKey(activeRepo) === key;
            const rowTone = statusTone(r.connection_status);
            return (
              <div key={key} className="flex items-center gap-0.5">
                <PopoverClose asChild>
                  <button
                    onClick={pick(key)}
                    className={`min-w-0 flex-1 text-left px-2 py-1.5 rounded hover:bg-surface-2 ${active ? "bg-surface-2" : ""}`}
                  >
                    <div className="flex items-center gap-1.5 font-medium">
                      <span className="truncate">{r.repo_full_name}</span>
                      {rowTone && (
                        <span
                          className={`h-1.5 w-1.5 shrink-0 rounded-full ${rowTone}`}
                          title={`Connection ${r.connection_status}`}
                        />
                      )}
                    </div>
                    <div className="text-xs text-fg-muted">
                      {r.bot_ids.length > 0
                        ? `${r.bot_ids.length} bot${r.bot_ids.length > 1 ? "s" : ""} enabled`
                        : "no bots enabled"}
                    </div>
                  </button>
                </PopoverClose>
                <PopoverClose asChild>
                  <button
                    onClick={() => navigate(repoDetailPath(r))}
                    className="shrink-0 rounded p-1.5 text-fg-subtle hover:bg-surface-2 hover:text-fg-default transition-colors"
                    title={`Repository details — ${r.repo_full_name}`}
                    aria-label={`Repository details — ${r.repo_full_name}`}
                  >
                    <ChevronRightIcon className="h-4 w-4" />
                  </button>
                </PopoverClose>
              </div>
            );
          })}
        </div>
      ))}
      <div className="my-1 border-t border-border-subtle" />
      <PopoverClose asChild>
        <button
          onClick={goConnect}
          className="flex w-full items-center gap-1.5 px-2 py-1.5 rounded hover:bg-surface-2 text-accent-text"
        >
          <PlusIcon className="h-4 w-4 shrink-0" />
          Connect a repo…
        </button>
      </PopoverClose>
    </Popover>
  );
}
