import { useCallback, useEffect, useState } from "react";

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
} from "@/api/marketplace";
import { listBots } from "@/api/bots";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { useUIStore } from "@/store/ui";
import { toastError } from "@/lib/errorHints";
import { useAuth } from "@/auth/AuthContext";
import { useLocation } from "wouter";

import { MarketplaceCard } from "./MarketplaceCard";
import { MarketplaceDetail } from "./MarketplaceDetail";
import { MarketplaceSubmit } from "./MarketplaceSubmit";
import { ModerationQueue } from "./ModerationQueue";
import {
  buildInstalledVersions,
  resolveInstalledState,
  type InstalledVersions,
} from "./installState";

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
  const { status } = useAuth();
  const anonymous = status === "anonymous";
  const [, navigate] = useLocation();
  const [entries, setEntries] = useState<MarketplaceEntry[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [search, setSearch] = useState("");
  const [tag, setTag] = useState("");
  const [activeSlug, setActiveSlug] = useState<string | null>(null);
  const [installing, setInstalling] = useState<string | null>(null);
  const [installed, setInstalled] = useState<InstalledVersions>(new Map());
  const [config, setConfig] = useState<MarketplaceConfig | null>(null);
  const [pending, setPending] = useState<MarketplaceEntry[]>([]);

  // Config drives the submit scope picker; it's static so fetch once.
  useEffect(() => {
    getMarketplaceConfig()
      .then(setConfig)
      .catch(() => setConfig(null));
  }, []);

  // Best-effort moderation queue — populated only for admins (the
  // endpoint 403s / 404s otherwise, leaving the section hidden). Used by
  // the mutation handlers to refetch after an approve/reject/submit.
  const refreshPending = useCallback(async () => {
    if (anonymous) {
      setPending([]);
      return;
    }
    try {
      setPending(await listModerationQueue());
    } catch {
      setPending([]);
    }
  }, [anonymous]);

  // Initial load via a promise chain (no synchronous setState in the
  // effect body) so the queue shows on first paint for admins. Skipped for
  // anonymous viewers (the endpoint is auth-only).
  useEffect(() => {
    // Anonymous viewers never load the (auth-only) moderation queue; the
    // initial empty state already hides the section.
    if (anonymous) return;
    listModerationQueue()
      .then(setPending)
      .catch(() => setPending([]));
  }, [anonymous]);

  // Best-effort: reconcile the registry against the bots already in the
  // workspace so cards can show Installed / Update. A failure (e.g. cloud
  // mode where install is disabled anyway) just leaves the map empty.
  const refreshInstalled = useCallback(async () => {
    // listBots is auth-gated; an anonymous viewer has no workspace to
    // reconcile against, so the install state is simply empty.
    if (anonymous) {
      setInstalled(new Map());
      return;
    }
    try {
      setInstalled(buildInstalledVersions(await listBots()));
    } catch {
      setInstalled(new Map());
    }
  }, [anonymous]);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [list] = await Promise.all([listMarketplace(search, tag), refreshInstalled()]);
      setEntries(list);
    } catch (e) {
      toastError(addToast, e, "Failed to load marketplace");
    } finally {
      setLoading(false);
    }
  }, [search, tag, addToast, refreshInstalled]);

  // Debounced refetch on search/tag changes so typing in the search box
  // doesn't fire a request per keystroke.
  useEffect(() => {
    const t = window.setTimeout(() => void refresh(), 200);
    return () => window.clearTimeout(t);
  }, [refresh]);

  // install (force=false) and update (force=true) share a path — both
  // copy the entry's bundle into .botz/, the only difference being whether
  // an existing install is overwritten.
  const onInstall = async (e: MarketplaceEntry, force = false) => {
    setInstalling(e.slug);
    try {
      const res = await installMarketplaceBot(e.slug, force);
      addToast(
        `${force ? "Updated" : "Installed"} ${res.install.name} → ${res.install.installed_path}`,
        "success",
      );
      // Reflect the bumped install counter without a full refetch.
      setEntries((prev) =>
        prev?.map((x) => (x.slug === e.slug ? res.entry : x)) ?? prev,
      );
      await refreshInstalled();
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
      await refreshInstalled();
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

  return (
    <div className="flex h-full min-h-0 flex-col bg-surface-1 text-fg-default">
      <header className="border-b border-border-default px-6 py-4">
        <div className="mx-auto flex max-w-5xl flex-col gap-1">
          <h1 className="text-base font-semibold text-fg-default">Bot marketplace</h1>
          <p className="text-xs text-fg-muted">
            Browse the hosted registry, submit a repository, or install a
            published bot into this workspace's <code className="text-fg-default">.botz/</code>.
          </p>
        </div>
      </header>

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
                aria-label="Search bots"
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
            <Button variant="secondary" size="sm" onClick={() => void refresh()}>
              Refresh
            </Button>
          </div>
        </section>

        {anonymous ? (
          <section className="flex flex-wrap items-center justify-between gap-3 rounded border border-border-default bg-surface-2 p-4">
            <span className="text-xs text-fg-muted">
              Want to publish a bot? Sign in with GitHub to submit a repository —
              submissions are reviewed before they appear here.
            </span>
            <Button variant="primary" size="sm" onClick={() => navigate("/")}>
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
          />
        )}

        {pending.length > 0 && (
          <ModerationQueue entries={pending} onApprove={onApprove} onReject={onReject} />
        )}

        <section className="flex flex-col gap-2">
          <div className="flex items-center justify-between">
            <h2 className="text-xs font-medium text-fg-muted">
              {entries === null
                ? "Loading bots…"
                : entries.length === 0
                  ? "No bots in the marketplace yet"
                  : `${entries.length} bot${entries.length === 1 ? "" : "s"}`}
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
            <ul className="grid grid-cols-1 gap-2 md:grid-cols-2">
              {entries.map((e) => (
                <MarketplaceCard
                  key={e.slug}
                  entry={e}
                  state={resolveInstalledState(e, installed)}
                  installing={installing === e.slug}
                  onInstall={() => void onInstall(e)}
                  onUpdate={() => void onInstall(e, true)}
                  onUninstall={() => void onUninstall(e)}
                  onOpen={() => setActiveSlug(e.slug)}
                  anonymous={anonymous}
                />
              ))}
            </ul>
          )}
        </section>
      </div>

      {active && (
        <MarketplaceDetail
          entry={active}
          state={resolveInstalledState(active, installed)}
          installing={installing === active.slug}
          onInstall={() => void onInstall(active)}
          onUpdate={() => void onInstall(active, true)}
          onUninstall={() => void onUninstall(active)}
          onClose={() => setActiveSlug(null)}
          anonymous={anonymous}
        />
      )}
    </div>
  );
}
