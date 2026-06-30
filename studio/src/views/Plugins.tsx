import { type ReactNode, useCallback, useEffect, useMemo, useState } from "react";
import { Component1Icon } from "@radix-ui/react-icons";

import {
  installPlugin,
  listPlugins,
  setPluginEnabled,
  uninstallPlugin,
  type PluginView,
} from "@/api/plugins";
import { useAuth } from "@/auth/AuthContext";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import { useConfirm } from "@/hooks/useConfirm";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { PageHeader } from "@/components/ui/PageHeader";
import { Spinner } from "@/components/ui/Spinner";
import PluginConfigForm from "./plugins/PluginConfigForm";

// Plugins lists the iterion plugin registry (embedded builtins +
// ~/.iterion/plugins) and lets the operator enable/disable each one, filter by
// contribution type, and — for super-admins (or the single-user local operator)
// — install new plugins and remove installed ones.
// Backed by GET /api/v1/plugins + POST /api/v1/plugins/{name}/{enable,disable}
// + POST /api/v1/plugins/install + DELETE /api/v1/plugins/{name}.
export default function Plugins() {
  const [plugins, setPlugins] = useState<PluginView[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [kindFilter, setKindFilter] = useState<string | null>(null);
  const [installOpen, setInstallOpen] = useState(false);
  const [configOpen, setConfigOpen] = useState<string | null>(null);

  const { user } = useAuth();
  const authRequired = useServerInfoStore((s) => s.info?.auth_required);
  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog } = useConfirm();
  // Install/remove mutate the shared plugin tree, so they're super-admin only —
  // mirroring the backend's requireSuperAdmin, which also passes for the
  // single-user local/desktop operator (auth disabled → no login).
  const canManage = (user?.is_super_admin ?? false) || authRequired === false;

  const refresh = useCallback(async () => {
    try {
      setError(null);
      setPlugins(await listPlugins());
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // The set of contribution kinds present across the registry, for the filter.
  const allKinds = useMemo(
    () => [...new Set((plugins ?? []).flatMap((p) => p.kinds))].sort(),
    [plugins],
  );
  // Derive the effective filter so a stale selection (e.g. the last plugin of
  // that kind was uninstalled) transparently falls back to "All" — no effect
  // syncing state to props.
  const activeKind = kindFilter && allKinds.includes(kindFilter) ? kindFilter : null;

  const filtered = useMemo(
    () =>
      activeKind
        ? (plugins ?? []).filter((p) => p.kinds.includes(activeKind))
        : (plugins ?? []),
    [plugins, activeKind],
  );

  const toggle = useCallback(
    async (p: PluginView) => {
      setBusy(p.name);
      try {
        await setPluginEnabled(p.name, !p.enabled);
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
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
        await refresh();
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      } finally {
        setBusy(null);
      }
    },
    [confirm, addToast, refresh],
  );

  return (
    <div className="flex h-full min-h-0 flex-col bg-surface-0 text-fg-default">
      {dialog}
      <PageHeader
        icon={<Component1Icon className="h-5 w-5" />}
        title="Plugins"
        description={
          <>
            Extend iterion with rewriters, MCP servers, skills, commands, agents
            and hooks. Builtins ship with the binary; install more from a git URL
            or local path.
          </>
        }
        actions={
          <div className="flex items-center gap-2">
            <Button variant="secondary" size="sm" onClick={() => void refresh()}>
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

      <div className="mx-auto flex w-full max-w-3xl flex-1 flex-col gap-3 overflow-y-auto p-6">
        {error && (
          <InlineBanner tone="danger" title="Plugin error" layout="inline">
            {error}
          </InlineBanner>
        )}

        {plugins === null && !error && (
          <EmptyState message="Loading plugins…" icon={<Spinner />} />
        )}

        {/* Filter by contribution type — only when there's more than one kind to
            choose between, so a single-kind registry stays uncluttered. */}
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

        {plugins !== null && plugins.length === 0 && (
          <EmptyState message="No plugins found." />
        )}
        {plugins !== null && plugins.length > 0 && filtered.length === 0 && (
          <EmptyState message={`No ${activeKind} plugins.`} />
        )}

        <ul className="flex flex-col gap-2">
          {filtered.map((p) => {
            const configurable = canManage && (p.config_schema?.length ?? 0) > 0;
            const open = configOpen === p.name;
            return (
              <li
                key={p.name}
                className="flex flex-col gap-3 rounded-[var(--radius-lg)] border border-border-default bg-surface-1 p-4 shadow-[var(--shadow-sm)] transition-[box-shadow,border-color] duration-[var(--motion-fast)] ease-[var(--motion-ease)] hover:border-border-strong hover:shadow-[var(--shadow-md)]"
              >
                <div className="flex items-start justify-between gap-4">
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-mono text-sm text-fg-default">{p.name}</span>
                      {p.version && (
                        <span className="text-caption text-fg-subtle">{p.version}</span>
                      )}
                      <Badge variant={p.builtin ? "info" : "neutral"} size="sm">
                        {p.builtin ? "builtin" : "installed"}
                      </Badge>
                      {p.enabled && (
                        <Badge variant="success" size="sm">
                          enabled
                        </Badge>
                      )}
                    </div>
                    {p.description && (
                      <p className="mt-1 text-caption text-fg-muted">{p.description}</p>
                    )}
                    <div className="mt-1.5 flex flex-wrap gap-1">
                      {p.kinds.map((k) => (
                        <Badge key={k} variant="accent" size="sm">
                          {k}
                        </Badge>
                      ))}
                    </div>
                  </div>
                  <div className="flex shrink-0 items-center gap-2">
                    {configurable && (
                      <Button
                        variant="ghost"
                        size="sm"
                        aria-expanded={open}
                        onClick={() => setConfigOpen(open ? null : p.name)}
                      >
                        {open ? "Close" : "Configure"}
                      </Button>
                    )}
                    {canManage && !p.builtin && (
                      <Button
                        variant="danger"
                        size="sm"
                        loading={busy === p.name}
                        disabled={busy !== null}
                        onClick={() => void remove(p)}
                      >
                        Remove
                      </Button>
                    )}
                    <Button
                      variant={p.enabled ? "secondary" : "primary"}
                      size="sm"
                      loading={busy === p.name}
                      disabled={busy !== null}
                      onClick={() => void toggle(p)}
                    >
                      {p.enabled ? "Disable" : "Enable"}
                    </Button>
                  </div>
                </div>
                {configurable && open && (
                  <div className="border-t border-border-default pt-3">
                    <PluginConfigForm plugin={p} onSaved={refresh} />
                  </div>
                )}
              </li>
            );
          })}
        </ul>

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
