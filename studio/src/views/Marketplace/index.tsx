import { useCallback, useEffect, useMemo, useState } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  approveModeration,
  getMarketplaceConfig,
  installMarketplaceBot,
  listMarketplace,
  listModerationQueue,
  rejectModeration,
  submitMarketplaceBot,
  uninstallMarketplaceBot,
  type MarketplaceConfig,
  type MarketplaceEntry,
  type MarketplaceKind,
  type MarketplaceSort,
} from "@/api/marketplace";
import { listBots } from "@/api/bots";
import { listPlugins } from "@/api/plugins";
import { ArchiveIcon } from "@radix-ui/react-icons";

import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { PageHeader } from "@/components/ui/PageHeader";
import { Select } from "@/components/ui/Select";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import { toastError } from "@/lib/errorHints";
import { useAuth } from "@/auth/AuthContext";
import { useLocation, useSearch } from "wouter";

import { MarketplaceCard } from "./MarketplaceCard";
import { MarketplaceDetail } from "./MarketplaceDetail";
import { MarketplaceSubmit } from "./MarketplaceSubmit";
import { ModerationQueue } from "./ModerationQueue";
import {
  buildInstalledPluginVersions,
  buildInstalledVersions,
  resolveInstalledState,
  type InstalledVersions,
} from "./installState";

// MarketplaceCategory groups entries for display. Bots are the first-class
// category (rendered first, most prominent); the plugin-type categories
// (rewriter / mcp / skill / command / agent / hook) follow.
interface MarketplaceCategory {
  key: string;
  label: string;
  entries: MarketplaceEntry[];
}

// CATEGORY_ORDER is the canonical taxonomy + display order. "bot" is the
// first-class category; the rest are plugin contribution kinds. "plugin" is the
// catch-all for a plugin entry that declares no recognised kind.
const CATEGORY_ORDER: { key: string; label: string }[] = [
  { key: "bot", label: "Bots" },
  { key: "rewriter", label: "Rewriters" },
  { key: "mcp", label: "MCP servers" },
  { key: "skill", label: "Skills" },
  { key: "command", label: "Commands" },
  { key: "agent", label: "Agents" },
  { key: "hook", label: "Hooks" },
  { key: "lifecycle", label: "Lifecycle" },
  { key: "plugin", label: "Plugins" },
];

// categoriesOf returns the category keys an entry belongs to. A bot (kind unset
// or "bot") is just "bot". A plugin lists its contribution kinds (carried in
// `categories`), falling back to the catch-all "plugin" when none are declared.
function categoriesOf(e: MarketplaceEntry): string[] {
  if ((e.kind ?? "bot") !== "plugin") return ["bot"];
  return e.categories && e.categories.length > 0 ? e.categories : ["plugin"];
}

// groupByCategory partitions entries into the canonical ordered categories,
// dropping empties. An entry contributing several kinds appears under each.
// Within a category the server's sort order is preserved.
function groupByCategory(entries: MarketplaceEntry[]): MarketplaceCategory[] {
  const out: MarketplaceCategory[] = [];
  for (const { key, label } of CATEGORY_ORDER) {
    const inCat = entries.filter((e) => categoriesOf(e).includes(key));
    if (inCat.length > 0) out.push({ key, label, entries: inCat });
  }
  return out;
}

// KIND_FILTERS are the kind filter pills. "" = no kind filter (All).
const KIND_FILTERS: { value: MarketplaceKind | ""; label: string }[] = [
  { value: "", label: "All" },
  { value: "bot", label: "Bots" },
  { value: "plugin", label: "Plugins" },
];

function parseKind(v: string | null): MarketplaceKind | "" {
  return v === "bot" || v === "plugin" ? v : "";
}

function parseSort(v: string | null): MarketplaceSort {
  return v === "recent" || v === "name" ? v : "popular";
}

// Stable empty install-state map for the anonymous / still-loading
// renders (never mutated), so downstream props keep a stable reference.
const EMPTY_VERSIONS: InstalledVersions = new Map();

/** MarketplaceView is the hosted bot registry browse / submit / install
 *  surface. Mirrors the studio's other view conventions: page header,
 *  centred content, neutral surfaces, accent for primary actions. The
 *  view is gated by `serverInfo.marketplace_enabled` at the route level
 *  so it only mounts when the server has the registry store wired. */
export default function MarketplaceView() {
  const addToast = useUIStore((s) => s.addToast);
  // Anonymous visitors (the public landing → /marketplace path) can browse +
  // download but not install/submit/moderate. Those calls hit auth-gated
  // endpoints, so we skip them entirely rather than swallow 401s.
  const { status, isRestricted } = useAuth();
  const anonymous = status === "anonymous";
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  // A user with no workspace (anonymous, the restricted submitter tier, OR
  // any cloud tenant — the server 403s install in cloud mode) can't install
  // into a workspace; they get the `.botz` download / CLI-copy path instead.
  // Restricted users ARE authenticated, so they still get the submit form.
  const noWorkspace = anonymous || isRestricted || cloud;
  const [locationPath, navigate] = useLocation();
  const searchString = useSearch();
  // Kind/sort initialize from the URL so /marketplace?kind=plugin
  // deep-links. Mount-only: later search-string changes come from our
  // own mirror effect below and must not clobber in-flight state.
  const initial = useMemo(() => {
    const p = new URLSearchParams(searchString);
    return { kind: parseKind(p.get("kind")), sort: parseSort(p.get("sort")) };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const [tag, setTag] = useState("");
  const [kind, setKind] = useState<MarketplaceKind | "">(initial.kind);
  const [sort, setSort] = useState<MarketplaceSort>(initial.sort);
  const [activeSlug, setActiveSlug] = useState<string | null>(null);
  const [installing, setInstalling] = useState<string | null>(null);

  // Mirror kind/sort back into the URL (replace semantics) so the pills
  // are bookmarkable without stacking history entries. Defaults are
  // dropped from the query string to keep the canonical URL clean.
  useEffect(() => {
    const p = new URLSearchParams(window.location.search);
    if (kind) p.set("kind", kind);
    else p.delete("kind");
    if (sort !== "popular") p.set("sort", sort);
    else p.delete("sort");
    const qs = p.toString();
    navigate(qs ? `${locationPath}?${qs}` : locationPath, { replace: true });
    // locationPath is read, not a trigger — re-navigating on every path
    // change would fight route transitions.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [kind, sort, navigate]);

  // Config drives the submit scope picker; it's static, and a failed
  // fetch just reads as "no config".
  const configQuery = useQuery<MarketplaceConfig>({
    queryKey: ["marketplace-config"],
    queryFn: getMarketplaceConfig,
  });
  const config = configQuery.data ?? null;

  // The moderation queue only exists on a moderated (cloud) registry —
  // config.moderated is the capability signal — and the endpoint is
  // auth-only, so `enabled` gating avoids a doomed 404 on every visit.
  // Best-effort: populated only for admins (a 403/404 leaves the section
  // hidden via the empty fallback).
  const moderated = config?.moderated ?? false;
  const moderationEnabled = !anonymous && moderated;
  const pendingQuery = useQuery<MarketplaceEntry[]>({
    queryKey: ["marketplace-moderation"],
    queryFn: listModerationQueue,
    enabled: moderationEnabled,
  });
  const pending = (moderationEnabled ? pendingQuery.data : undefined) ?? [];

  // Used by the mutation handlers to refetch the queue after an
  // approve/reject/submit (resolves once the active query refetched).
  const refreshPending = useCallback(async () => {
    await queryClient.invalidateQueries({ queryKey: ["marketplace-moderation"] });
  }, [queryClient]);

  // Best-effort: reconcile the registry against the bots already in the
  // workspace (.botz/) and the plugins in ~/.iterion/plugins/ so cards
  // can show Installed / Update. Each fetch fails independently (e.g.
  // cloud mode where install is disabled anyway) to an empty map. Both
  // endpoints are auth-gated; an anonymous viewer has no workspace to
  // reconcile against, so the query is disabled and the maps stay empty.
  const installedQuery = useQuery({
    queryKey: ["marketplace-installed"],
    queryFn: async () => {
      const empty = (): InstalledVersions => new Map();
      const [bots, plugins] = await Promise.all([
        listBots().then(buildInstalledVersions).catch(empty),
        listPlugins().then(buildInstalledPluginVersions).catch(empty),
      ]);
      return { bots, plugins };
    },
    enabled: !anonymous,
  });
  const installed =
    (anonymous ? undefined : installedQuery.data?.bots) ?? EMPTY_VERSIONS;
  const installedPlugins =
    (anonymous ? undefined : installedQuery.data?.plugins) ?? EMPTY_VERSIONS;

  // Debounce the filters into the query key so typing in the search box
  // doesn't fire a request per keystroke.
  const [filters, setFilters] = useState(() => ({
    search: "",
    tag: "",
    kind: initial.kind,
    sort: initial.sort,
  }));
  useEffect(() => {
    const t = window.setTimeout(() => setFilters({ search, tag, kind, sort }), 200);
    return () => window.clearTimeout(t);
  }, [search, tag, kind, sort]);

  // keepPreviousData: filter/sort changes keep the current list on
  // screen under the "Refreshing…" hint instead of flashing back to the
  // loading header.
  const entriesQuery = useQuery<MarketplaceEntry[]>({
    queryKey: ["marketplace", filters.search, filters.tag, filters.kind, filters.sort],
    queryFn: () =>
      listMarketplace(filters.search, filters.tag, filters.kind, filters.sort),
    placeholderData: keepPreviousData,
  });
  const entries = entriesQuery.data ?? null;
  const loading = entriesQuery.isFetching;

  // Load failures surface as a toast (the browse list is the page's core
  // surface — a banner would displace it), once per failed load.
  const entriesError = entriesQuery.error;
  useEffect(() => {
    if (entriesError) toastError(addToast, entriesError, "Failed to load marketplace");
  }, [entriesError, addToast]);

  // Full reload (Refresh button + post-submit/approve/upload): browse
  // list and installed reconcile together, like the pre-query refresh.
  // invalidate — not refetch — so the disabled installed query
  // (anonymous) stays a no-op. ["marketplace"] prefix-matches every
  // cached filter combination but not marketplace-config/-moderation.
  const refresh = useCallback(async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["marketplace"] }),
      queryClient.invalidateQueries({ queryKey: ["marketplace-installed"] }),
    ]);
  }, [queryClient]);

  // install (force=false) and update (force=true) share a path — a bot
  // entry's bundle lands in the workspace .botz/, a plugin entry in
  // ~/.iterion/plugins/; the only difference is whether an existing
  // install is overwritten.
  const onInstall = async (e: MarketplaceEntry, force = false) => {
    setInstalling(e.slug);
    try {
      const res = await installMarketplaceBot(e.slug, force);
      if (res.plugin) {
        // Plugin installs land in ~/.iterion/plugins/ and are managed
        // (enabled/configured) from the Plugins page.
        addToast(
          `${force ? "Updated" : "Installed"} ${res.plugin.name} — enable it from the Plugins page`,
          "success",
        );
      } else if (res.install) {
        addToast(
          `${force ? "Updated" : "Installed"} ${res.install.name} → ${res.install.installed_path}`,
          "success",
        );
      } else {
        addToast(`${force ? "Updated" : "Installed"} ${e.name}`, "success");
      }
      // Reflect the bumped install counter without a full refetch.
      queryClient.setQueryData<MarketplaceEntry[]>(
        ["marketplace", filters.search, filters.tag, filters.kind, filters.sort],
        (prev) => prev?.map((x) => (x.slug === e.slug ? res.entry : x)),
      );
      await queryClient.invalidateQueries({ queryKey: ["marketplace-installed"] });
    } catch (err) {
      toastError(addToast, err, force ? "Update failed" : "Install failed");
    } finally {
      setInstalling(null);
    }
  };

  const onUninstall = async (e: MarketplaceEntry) => {
    setInstalling(e.slug);
    try {
      await uninstallMarketplaceBot(e.slug);
      addToast(`Uninstalled ${e.name}`, "success");
      await queryClient.invalidateQueries({ queryKey: ["marketplace-installed"] });
    } catch (err) {
      toastError(addToast, err, "Uninstall failed");
    } finally {
      setInstalling(null);
    }
  };

  const onSubmit = async (req: {
    repo_url: string;
    ref?: string;
    path?: string;
    tags?: string[];
    icon?: string;
    scope?: MarketplaceEntry["scope"];
  }) => {
    try {
      const stored = await submitMarketplaceBot(req);
      const queued = config?.moderated && stored.status === "pending";
      addToast(
        queued
          ? `Submitted "${stored.display_name || stored.name}" for review`
          : `Added "${stored.display_name || stored.name}" to the marketplace`,
        "success",
      );
      await refresh();
      await refreshPending();
      if (!queued) setActiveSlug(stored.slug);
    } catch (e) {
      toastError(addToast, e, "Submission failed");
      throw e;
    }
  };

  const onApprove = async (slug: string) => {
    try {
      await approveModeration(slug);
      addToast("Approved", "success");
      await Promise.all([refresh(), refreshPending()]);
    } catch (e) {
      toastError(addToast, e, "Approve failed");
    }
  };

  const onReject = async (slug: string, reason: string) => {
    try {
      await rejectModeration(slug, reason);
      addToast("Rejected", "info");
      await refreshPending();
    } catch (e) {
      toastError(addToast, e, "Reject failed");
    }
  };

  const active = entries?.find((e) => e.slug === activeSlug) ?? null;

  // Featured strip: the top entries by adoption, shown only on the
  // unfiltered browse view (any q/tag/kind filter replaces it with the
  // filtered result). The server's default sort is popularity, so the
  // head of the list is already "most installed".
  const unfiltered = search.trim() === "" && tag.trim() === "" && kind === "";
  const featured = unfiltered
    ? (entries ?? []).filter((e) => e.installs > 0).slice(0, 4)
    : [];

  return (
    <div className="flex h-full min-h-0 flex-col bg-surface-0 text-fg-default">
      <PageHeader
        icon={<ArchiveIcon className="h-5 w-5" />}
        title="Marketplace"
        description={
          noWorkspace ? (
            <>
              Browse the hosted registry, submit a repository, or grab a
              published bot as a{" "}
              <code className="font-mono text-fg-default">.botz</code> download
              — plugins install locally with the{" "}
              <code className="font-mono text-fg-default">iterion</code> CLI.
            </>
          ) : (
            <>
              Browse the hosted registry, submit a repository, or install a
              published bot (into this workspace's{" "}
              <code className="font-mono text-fg-default">.botz/</code>) or
              plugin (into{" "}
              <code className="font-mono text-fg-default">~/.iterion/plugins/</code>).
            </>
          )
        }
        actions={
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void refresh()}
            loading={loading}
          >
            Refresh
          </Button>
        }
      />

      <div className="mx-auto flex w-full max-w-5xl flex-1 flex-col gap-4 overflow-y-auto px-6 py-4">
        <section className="flex flex-col gap-2">
          <div className="flex flex-wrap items-end gap-2">
            <label htmlFor="marketplace-search" className="flex min-w-[14rem] flex-1 flex-col gap-1">
              <span className="text-caption uppercase tracking-wide text-fg-subtle">Search</span>
              <Input
                id="marketplace-search"
                type="text"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="name, description, tag…"
                aria-label="Search marketplace"
              />
            </label>
            <label htmlFor="marketplace-tag" className="flex w-44 flex-col gap-1">
              <span className="text-caption uppercase tracking-wide text-fg-subtle">Filter by tag</span>
              <Input
                id="marketplace-tag"
                type="text"
                value={tag}
                onChange={(e) => setTag(e.target.value)}
                placeholder="(e.g. review)"
                aria-label="Filter by tag"
              />
            </label>
            <label htmlFor="marketplace-sort" className="flex w-32 flex-col gap-1">
              <span className="text-caption uppercase tracking-wide text-fg-subtle">Sort</span>
              <Select
                id="marketplace-sort"
                value={sort}
                onChange={(e) => setSort(e.target.value as MarketplaceSort)}
                aria-label="Sort entries"
              >
                <option value="popular">Popular</option>
                <option value="recent">Recent</option>
                <option value="name">Name</option>
              </Select>
            </label>
          </div>
          <div
            role="group"
            aria-label="Filter by kind"
            className="flex flex-wrap items-center gap-1"
          >
            {KIND_FILTERS.map(({ value, label }) => (
              <button
                key={value || "all"}
                type="button"
                aria-pressed={kind === value}
                onClick={() => setKind(value)}
                className={`rounded-full border px-2.5 py-0.5 text-xs transition-colors focus:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
                  kind === value
                    ? "border-accent bg-accent-soft text-fg-default"
                    : "border-border-default bg-surface-1 text-fg-muted hover:bg-surface-2 hover:text-fg-default"
                }`}
              >
                {label}
              </button>
            ))}
          </div>
        </section>

        {anonymous ? (
          <section className="flex flex-wrap items-center justify-between gap-3 rounded border border-border-default bg-surface-2 p-4">
            <span className="text-xs text-fg-muted">
              Want to publish a bot? Sign in with GitHub to submit a repository —
              submissions are reviewed before they appear here.
            </span>
            <Button variant="primary" size="sm" onClick={() => navigate("/login")}>
              Sign in to propose a bot
            </Button>
          </section>
        ) : (
          <MarketplaceSubmit
            onSubmit={onSubmit}
            onUploaded={() => void refresh()}
            scopes={config?.scopes}
            defaultScope={config?.default_scope}
            moderated={config?.moderated}
            cloud={cloud}
          />
        )}

        {pending.length > 0 && (
          <ModerationQueue entries={pending} onApprove={onApprove} onReject={onReject} />
        )}

        <section className="flex flex-col gap-2">
          <div className="flex items-center justify-between">
            <h2 className="text-xs font-medium text-fg-muted">
              {entries === null
                ? "Loading marketplace…"
                : entries.length === 0
                  ? "Nothing in the marketplace yet"
                  : `${entries.length} entr${entries.length === 1 ? "y" : "ies"}`}
            </h2>
            {loading && entries && entries.length > 0 && (
              <span className="text-caption text-fg-subtle">Refreshing…</span>
            )}
          </div>
          {entries === null ? null : entries.length === 0 ? (
            <div className="rounded border border-border-default bg-surface-2 p-4 text-xs text-fg-muted">
              {anonymous ? (
                <>
                  No bots published yet. Sign in to submit a repository — once
                  reviewed, it appears here for anyone to download.
                </>
              ) : noWorkspace ? (
                <>
                  Use the form above to submit a repository. Submission validates
                  the bundle and indexes its metadata; published entries appear
                  here as <code className="font-mono text-fg-default">.botz</code>{" "}
                  downloads and CLI install commands.
                </>
              ) : (
                <>
                  Use the form above to submit a repository. Submission validates
                  the bundle and indexes its metadata; nothing is installed until
                  you click <span className="text-fg-default">Install</span> on its
                  card.
                </>
              )}
            </div>
          ) : (
            <div className="flex flex-col gap-6">
              {featured.length > 0 && (
                <section className="flex flex-col gap-2">
                  <div className="flex items-baseline gap-2 border-b border-border-default pb-1">
                    <h3 className="text-sm font-semibold text-fg-default">Featured</h3>
                    <span className="text-caption text-fg-subtle">most installed</span>
                  </div>
                  <ul className="grid grid-cols-2 gap-2 md:grid-cols-4">
                    {featured.map((e) => (
                      <li key={e.slug} className="min-w-0">
                        <button
                          type="button"
                          onClick={() => setActiveSlug(e.slug)}
                          className="flex h-full w-full flex-col items-start gap-1 rounded-[var(--radius-lg)] border border-border-default bg-surface-1 p-3 text-left shadow-[var(--shadow-sm)] transition-[box-shadow,border-color,transform] duration-[var(--motion-fast)] ease-[var(--motion-ease)] hover:-translate-y-0.5 hover:border-border-strong hover:shadow-[var(--shadow-md)] focus:outline-none focus-visible:ring-1 focus-visible:ring-accent"
                        >
                          <div className="flex w-full min-w-0 items-center gap-1.5">
                            {e.icon && (
                              <span aria-hidden className="shrink-0 text-base leading-none">
                                {e.icon}
                              </span>
                            )}
                            <span className="truncate text-xs font-medium text-fg-default">
                              {e.display_name?.trim() || e.name}
                            </span>
                          </div>
                          <span className="text-caption text-fg-subtle">
                            {(e.kind ?? "bot") === "plugin" ? "Plugin · " : ""}
                            {e.installs} install{e.installs === 1 ? "" : "s"}
                          </span>
                        </button>
                      </li>
                    ))}
                  </ul>
                </section>
              )}
              {groupByCategory(entries).map(({ key, label, entries: catEntries }) => (
                <section key={key} className="flex flex-col gap-2">
                  <div className="flex items-baseline gap-2 border-b border-border-default pb-1">
                    <h3
                      className={
                        key === "bot"
                          ? "text-sm font-semibold text-fg-default"
                          : "text-xs font-medium text-fg-muted"
                      }
                    >
                      {label}
                    </h3>
                    <span className="text-caption text-fg-subtle">
                      {catEntries.length}
                    </span>
                  </div>
                  {/* One entry per row. */}
                  <ul className="flex flex-col gap-2">
                    {catEntries.map((e) => (
                      <MarketplaceCard
                        key={e.slug}
                        entry={e}
                        state={resolveInstalledState(e, installed, installedPlugins)}
                        installing={installing === e.slug}
                        onInstall={() => void onInstall(e)}
                        onUpdate={() => void onInstall(e, true)}
                        onUninstall={() => void onUninstall(e)}
                        onOpen={() => setActiveSlug(e.slug)}
                        anonymous={noWorkspace}
                      />
                    ))}
                  </ul>
                </section>
              ))}
            </div>
          )}
        </section>
      </div>

      {active && (
        <MarketplaceDetail
          entry={active}
          state={resolveInstalledState(active, installed, installedPlugins)}
          installing={installing === active.slug}
          onInstall={() => void onInstall(active)}
          onUpdate={() => void onInstall(active, true)}
          onUninstall={() => void onUninstall(active)}
          onClose={() => setActiveSlug(null)}
          anonymous={noWorkspace}
          cloud={cloud}
        />
      )}
    </div>
  );
}
