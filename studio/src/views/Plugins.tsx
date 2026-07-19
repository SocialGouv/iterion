import { type ReactNode, useCallback, useMemo, useState } from "react";
import { Component1Icon } from "@radix-ui/react-icons";
import { useLocation } from "wouter";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import {
  installPlugin,
  listPlugins,
  setPluginEnabled,
  uninstallPlugin,
  type PluginView,
} from "@/api/plugins";
import { useAuth } from "@/auth/AuthContext";
import { errorMessage } from "@/lib/errorHints";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import { useConfirm } from "@/hooks/useConfirm";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { PageHeader } from "@/components/ui/PageHeader";
import { Spinner } from "@/components/ui/Spinner";
import PluginCard from "./plugins/PluginCard";
import PluginDetail from "./plugins/PluginDetail";

// Plugins lists the iterion plugin registry (embedded builtins +
// ~/.iterion/plugins) as a card gallery and lets the operator enable/disable
// each one, filter by contribution type, search, and — for super-admins (or
// the single-user local operator) — install new plugins and remove installed
// ones. Clicking a card opens a detail drawer (contributes/config/lifecycle/
// README) backed by GET /api/v1/plugins/{name}.
export default function Plugins() {
  const queryClient = useQueryClient();
  const pluginsQuery = useQuery<PluginView[]>({
    queryKey: ["plugins"],
    queryFn: listPlugins,
  });
  const plugins = pluginsQuery.data ?? null;
  // Enable/disable/remove failures share the fetch error's banner; any
  // reload clears them (the fetch side clears itself on refetch).
  const [actionError, setActionError] = useState<string | null>(null);
  const error =
    actionError ??
    (pluginsQuery.error && !pluginsQuery.isFetching
      ? errorMessage(pluginsQuery.error)
      : null);
  const [busy, setBusy] = useState<string | null>(null);
  const [kindFilter, setKindFilter] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [installOpen, setInstallOpen] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);

  const { user } = useAuth();
  const authRequired = useServerInfoStore((s) => s.info?.auth_required);
  const marketplaceEnabled = useServerInfoStore((s) => s.info?.marketplace_enabled);
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog } = useConfirm();
  const [, navigate] = useLocation();
  // Install/remove mutate the shared plugin tree, so they're super-admin only —
  // mirroring the backend's requireSuperAdmin, which also passes for the
  // single-user local/desktop operator (auth disabled → no login).
  const canManage = (user?.is_super_admin ?? false) || authRequired === false;

  // Post-mutation reload: invalidate so the registry list refetches
  // (awaitable — resolves once the active query has refetched).
  const refresh = useCallback(async () => {
    setActionError(null);
    await queryClient.invalidateQueries({ queryKey: ["plugins"] });
  }, [queryClient]);

  // The set of contribution kinds present across the registry, for the filter.
  const allKinds = useMemo(
    () => [...new Set((plugins ?? []).flatMap((p) => p.kinds))].sort(),
    [plugins],
  );
  // Derive the effective filter so a stale selection (e.g. the last plugin of
  // that kind was uninstalled) transparently falls back to "All" — no effect
  // syncing state to props.
  const activeKind = kindFilter && allKinds.includes(kindFilter) ? kindFilter : null;

  const filtered = useMemo(() => {
    let list = plugins ?? [];
    if (activeKind) list = list.filter((p) => p.kinds.includes(activeKind));
    const q = search.trim().toLowerCase();
    if (q) {
      list = list.filter((p) =>
        [p.name, p.description ?? "", ...p.kinds].join(" ").toLowerCase().includes(q),
      );
    }
    return list;
  }, [plugins, activeKind, search]);

  const toggle = useCallback(
    async (p: PluginView) => {
      setBusy(p.name);
      try {
        await setPluginEnabled(p.name, !p.enabled);
        await refresh();
      } catch (e) {
        setActionError(errorMessage(e));
      } finally {
        setBusy(null);
      }
    },
    [refresh],
  );

  const remove = useCallback(
    async (p: PluginView) => {
      const ok = await confirm({
        title: `Remove ${p.name}?`,
        message: `Uninstall the "${p.name}" plugin from ~/.iterion/plugins? This deletes its files. Builtins can only be disabled, not removed.`,
        confirmLabel: "Remove plugin",
        confirmVariant: "danger",
      });
      if (!ok) return;
      setBusy(p.name);
      try {
        await uninstallPlugin(p.name);
        addToast(`Removed plugin ${p.name}`, "success");
        setSelected((s) => (s === p.name ? null : s));
        await refresh();
      } catch (e) {
        setActionError(errorMessage(e));
      } finally {
        setBusy(null);
      }
    },
    [confirm, addToast, refresh],
  );

  const browseMarketplace = marketplaceEnabled ? (
    <Button
      variant="secondary"
      size="sm"
      onClick={() => navigate("/marketplace?kind=plugin")}
    >
      Browse marketplace
    </Button>
  ) : null;

  const selectedPlugin =
    selected !== null ? (plugins ?? []).find((p) => p.name === selected) : undefined;

  return (
    <div className="flex h-full min-h-0 flex-col bg-surface-0 text-fg-default">
      {dialog}
      <PageHeader
        icon={<Component1Icon className="h-5 w-5" />}
        title="Plugins"
        description={
          <>
            Plugins extend what every bot run can do — compress command output
            (rewriters), reach external tools (MCP servers), or ship extra
            skills, commands, agents and hooks. Builtins ship with the binary;
            install more from a git URL or local path.
          </>
        }
        actions={
          <div className="flex items-center gap-2">
            {browseMarketplace}
            <Button
              variant="secondary"
              size="sm"
              onClick={() => {
                setActionError(null);
                void pluginsQuery.refetch();
              }}
            >
              Refresh
            </Button>
            {canManage && (
              <Button variant="primary" size="sm" onClick={() => setInstallOpen(true)}>
                Install plugin
              </Button>
            )}
          </div>
        }
      />

      <div className="mx-auto flex w-full max-w-5xl flex-1 flex-col gap-3 overflow-y-auto p-6">
        {cloud && (
          <InlineBanner tone="info" layout="inline">
            Plugins are shared across this server — enabling or disabling one
            affects every workspace on the instance
            {canManage ? "." : ", so only administrators can change them."}
          </InlineBanner>
        )}
        {error && (
          <InlineBanner tone="danger" title="Plugin error" layout="inline">
            {error}
          </InlineBanner>
        )}

        {plugins === null && !error && (
          <EmptyState message="Loading plugins…" icon={<Spinner />} />
        )}

        {plugins !== null && plugins.length > 0 && (
          <div className="flex flex-wrap items-center gap-2">
            <Input
              size="md"
              type="search"
              placeholder="Search plugins…"
              aria-label="Search plugins"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              className="w-full max-w-xs"
            />
            {/* Filter by contribution type — only when there's more than one
                kind to choose between, so a single-kind registry stays
                uncluttered. */}
            {allKinds.length > 1 && (
              <div className="flex flex-wrap items-center gap-1">
                <span className="mr-1 text-caption text-fg-subtle">Type:</span>
                <FilterPill active={activeKind === null} onClick={() => setKindFilter(null)}>
                  All
                </FilterPill>
                {allKinds.map((k) => (
                  <FilterPill key={k} active={activeKind === k} onClick={() => setKindFilter(k)}>
                    {k}
                  </FilterPill>
                ))}
              </div>
            )}
          </div>
        )}

        {plugins !== null && plugins.length === 0 && (
          <EmptyState
            message="No plugins found."
            action={browseMarketplace ?? undefined}
          />
        )}
        {plugins !== null && plugins.length > 0 && filtered.length === 0 && (
          <EmptyState
            message={
              search.trim()
                ? `No plugins match “${search.trim()}”.`
                : `No ${activeKind} plugins.`
            }
          />
        )}

        {filtered.length > 0 && (
          <ul className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {filtered.map((p) => (
              <PluginCard
                key={p.name}
                plugin={p}
                busy={busy === p.name}
                canManage={canManage}
                onEnable={() => void toggle(p)}
                onOpen={() => setSelected(p.name)}
              />
            ))}
          </ul>
        )}

        {canManage && (
          <InstallPluginDialog
            open={installOpen}
            onOpenChange={setInstallOpen}
            onInstalled={(name) => {
              addToast(`Installed plugin ${name}`, "success");
              void refresh();
            }}
          />
        )}

        {selectedPlugin && (
          <PluginDetail
            plugin={selectedPlugin}
            canManage={canManage}
            busy={busy === selectedPlugin.name}
            onToggle={() => void toggle(selectedPlugin)}
            onRemove={() => void remove(selectedPlugin)}
            onClose={() => setSelected(null)}
            onConfigSaved={() => void refresh()}
          />
        )}
      </div>
    </div>
  );
}

function FilterPill({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={`rounded-full border px-2.5 py-0.5 text-caption font-medium transition-colors ${
        active
          ? "border-accent/40 bg-accent-soft text-fg-default"
          : "border-border-default text-fg-muted hover:bg-surface-2 hover:text-fg-default"
      }`}
    >
      {children}
    </button>
  );
}

function InstallPluginDialog({
  open,
  onOpenChange,
  onInstalled,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onInstalled: (name: string) => void;
}) {
  const [source, setSource] = useState("");
  const [installing, setInstalling] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const reset = () => {
    setSource("");
    setErr(null);
    setInstalling(false);
  };

  const submit = async () => {
    const src = source.trim();
    if (!src) return;
    setInstalling(true);
    setErr(null);
    try {
      const res = await installPlugin(src);
      onInstalled(res.name);
      reset();
      onOpenChange(false);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setInstalling(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) reset();
        onOpenChange(o);
      }}
      title="Install plugin"
      description="Clone a plugin from a git URL, or install from a local path on the server. A bare skills repo (no plugin.yaml) is wrapped as a skills-only plugin."
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={() => onOpenChange(false)} disabled={installing}>
            Cancel
          </Button>
          <Button
            variant="primary"
            size="sm"
            loading={installing}
            disabled={!source.trim()}
            onClick={() => void submit()}
          >
            Install
          </Button>
        </>
      }
    >
      <div className="space-y-2">
        <Input
          size="md"
          autoFocus
          placeholder="https://github.com/org/plugin.git  or  /path/to/plugin"
          value={source}
          onChange={(e) => setSource(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void submit();
          }}
          error={!!err}
        />
        {err && (
          <InlineBanner tone="danger" layout="inline">
            {err}
          </InlineBanner>
        )}
        <p className="text-caption text-fg-subtle">
          The source is cloned server-side; its <code>.git</code> metadata is
          stripped and nothing is executed during install. Enable it after it
          appears in the list.
        </p>
      </div>
    </Dialog>
  );
}
