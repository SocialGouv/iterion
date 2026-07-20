import { useMemo, useState } from "react";
import {
  ChevronDownIcon,
  ChevronRightIcon,
  CubeIcon,
  MagnifyingGlassIcon,
  LockClosedIcon,
} from "@radix-ui/react-icons";

import type { EditorShare } from "@/api/configEditor";
import { EmptyState } from "@/components/ui";

// ---------------------------------------------------------------------------
// ShareBrowser — the exploration surface for a team's config-shares. Scales
// past a flat list: a search box + collapsible repo → bot groups with counts,
// so browsing "by repo" / "by bot" is real navigation, not just labels. The
// group holding the selected share is always open; search filters across every
// level (repo, bot, category, label, path).
// ---------------------------------------------------------------------------

// shortRepo reduces a repo URL to "org/repo" for a compact group header.
function shortRepo(url: string): string {
  if (!url) return "Other";
  const cleaned = url.replace(/\.git$/, "").replace(/\/+$/, "");
  const m = cleaned.match(/([^/]+\/[^/]+)$/);
  return m?.[1] ?? cleaned;
}

interface BotGroup {
  botId: string;
  botLabel: string;
  shares: EditorShare[];
}
interface RepoGroup {
  repoKey: string;
  repoLabel: string;
  count: number;
  bots: BotGroup[];
}

// groupShares builds the repo → bot → shares hierarchy. Insertion order is
// preserved (the server already sorts).
function groupShares(shares: EditorShare[]): RepoGroup[] {
  const repos = new Map<string, Map<string, EditorShare[]>>();
  for (const s of shares) {
    const rk = s.repo_url ?? "";
    const bk = s.bot_id ?? "";
    let bots = repos.get(rk);
    if (!bots) {
      bots = new Map();
      repos.set(rk, bots);
    }
    const bucket = bots.get(bk);
    if (bucket) bucket.push(s);
    else bots.set(bk, [s]);
  }
  return Array.from(repos, ([rk, bots]) => {
    const botGroups = Array.from(bots, ([bk, ss]) => ({
      botId: bk,
      // Name the bot group by its persona (display_name, e.g. "Vigie"); fall
      // back to the editor surface title, then the technical bot id.
      botLabel: ss[0]?.bot_display || ss[0]?.editor_title || bk || "Config",
      shares: ss,
    }));
    return {
      repoKey: rk,
      repoLabel: shortRepo(rk),
      count: botGroups.reduce((n, b) => n + b.shares.length, 0),
      bots: botGroups,
    };
  });
}

function shareMatches(s: EditorShare, q: string): boolean {
  if (!q) return true;
  const hay = [
    s.label,
    s.category,
    s.bot_id,
    s.bot_display,
    s.editor_title,
    s.config_path,
    s.repo_url ? shortRepo(s.repo_url) : "",
  ]
    .join(" ")
    .toLowerCase();
  return hay.includes(q);
}

export function ShareBrowser({
  shares,
  selectedId,
  onSelect,
}: {
  shares: EditorShare[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  const [query, setQuery] = useState("");
  // Group keys the user explicitly collapsed. Default = everything open.
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const q = query.trim().toLowerCase();

  const groups = useMemo(() => {
    const all = groupShares(shares);
    if (!q) return all;
    // Filter to matching shares, dropping empty bots/repos.
    return all
      .map((repo) => ({
        ...repo,
        bots: repo.bots
          .map((bot) => ({ ...bot, shares: bot.shares.filter((s) => shareMatches(s, q)) }))
          .filter((bot) => bot.shares.length > 0),
      }))
      .map((repo) => ({ ...repo, count: repo.bots.reduce((n, b) => n + b.shares.length, 0) }))
      .filter((repo) => repo.bots.length > 0);
  }, [shares, q]);

  const toggle = (key: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  const repoTotal = new Set(shares.map((s) => s.repo_url ?? "")).size;
  const botTotal = new Set(shares.map((s) => s.bot_id ?? "")).size;

  return (
    <div className="flex flex-col gap-2" aria-label="Config-shares">
      <label className="relative block">
        <MagnifyingGlassIcon className="pointer-events-none absolute left-2.5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-fg-subtle" />
        <input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Filter by repo, bot, category…"
          aria-label="Filter config-shares"
          className="w-full rounded-md border border-border-default bg-surface-1 py-1.5 pl-8 pr-2 text-sm text-fg-default placeholder:text-fg-subtle focus:border-accent focus:outline-none"
        />
      </label>
      <div className="px-0.5 text-caption text-fg-subtle">
        {shares.length} config-share{shares.length === 1 ? "" : "s"} · {repoTotal} repo
        {repoTotal === 1 ? "" : "s"} · {botTotal} bot{botTotal === 1 ? "" : "s"}
      </div>

      {groups.length === 0 ? (
        <div className="pt-2">
          <EmptyState message={q ? `No config-share matches "${query}".` : "No config-shares."} />
        </div>
      ) : (
        <div className="flex flex-col gap-1.5">
          {groups.map((repo) => {
            const rKey = `repo:${repo.repoKey}`;
            const repoOpen = q !== "" || !collapsed.has(rKey);
            return (
              <div key={rKey} className="flex flex-col">
                <GroupHeader
                  open={repoOpen}
                  onToggle={() => toggle(rKey)}
                  title={repo.repoLabel}
                  titleAttr={repo.repoKey}
                  count={repo.count}
                  mono
                />
                {repoOpen && (
                  <div className="mt-1 flex flex-col gap-1.5 border-l border-border-subtle pl-2">
                    {repo.bots.map((bot) => {
                      const bKey = `bot:${repo.repoKey}/${bot.botId}`;
                      const hasSelected = bot.shares.some((s) => s.id === selectedId);
                      const botOpen = q !== "" || hasSelected || !collapsed.has(bKey);
                      return (
                        <div key={bKey} className="flex flex-col">
                          <GroupHeader
                            open={botOpen}
                            onToggle={() => toggle(bKey)}
                            icon={<CubeIcon className="h-3.5 w-3.5 shrink-0 text-fg-subtle" />}
                            title={bot.botLabel}
                            subtitle={bot.botLabel !== bot.botId ? bot.botId : undefined}
                            count={bot.shares.length}
                          />
                          {botOpen && (
                            <ul className="mt-1 flex flex-col gap-1 pl-1.5">
                              {bot.shares.map((s) => (
                                <li key={s.id}>
                                  <ShareLeaf
                                    share={s}
                                    active={s.id === selectedId}
                                    onSelect={() => onSelect(s.id)}
                                  />
                                </li>
                              ))}
                            </ul>
                          )}
                        </div>
                      );
                    })}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

function GroupHeader({
  open,
  onToggle,
  icon,
  title,
  titleAttr,
  subtitle,
  count,
  mono,
}: {
  open: boolean;
  onToggle: () => void;
  icon?: React.ReactNode;
  title: string;
  titleAttr?: string;
  subtitle?: string;
  count: number;
  mono?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-expanded={open}
      className="group flex w-full items-center gap-1.5 rounded-md px-1 py-1 text-left hover:bg-surface-2"
    >
      {open ? (
        <ChevronDownIcon className="h-3.5 w-3.5 shrink-0 text-fg-subtle" />
      ) : (
        <ChevronRightIcon className="h-3.5 w-3.5 shrink-0 text-fg-subtle" />
      )}
      {icon}
      <span
        className={`truncate font-medium text-fg-default ${mono ? "font-mono text-caption uppercase tracking-wide text-fg-muted" : "text-sm"}`}
        title={titleAttr}
      >
        {title}
      </span>
      {subtitle && (
        <span className="truncate font-mono text-caption text-fg-subtle">{subtitle}</span>
      )}
      <span className="ml-auto shrink-0 rounded-full bg-surface-2 px-1.5 text-caption text-fg-subtle group-hover:bg-surface-3">
        {count}
      </span>
    </button>
  );
}

function ShareLeaf({
  share: s,
  active,
  onSelect,
}: {
  share: EditorShare;
  active: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-current={active ? "true" : undefined}
      className={`w-full rounded-md border px-2.5 py-1.5 text-left transition-colors ${
        active
          ? "border-accent bg-accent-soft/50"
          : "border-transparent hover:border-border-default hover:bg-surface-1"
      }`}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="truncate text-sm font-medium text-fg-default">
          {s.label || s.category || s.id}
        </span>
        {s.read_only && (
          <LockClosedIcon className="h-3 w-3 shrink-0 text-fg-subtle" aria-label="read-only" />
        )}
      </div>
      <div className="mt-0.5 flex flex-wrap items-center gap-x-1.5 text-caption text-fg-subtle">
        {s.category && <span className="truncate">{s.category}</span>}
        {s.config_path && (
          <>
            <span aria-hidden>·</span>
            <span className="truncate font-mono">{s.config_path}</span>
          </>
        )}
      </div>
    </button>
  );
}
